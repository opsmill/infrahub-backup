package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/PlakarKorp/kloset/snapshot/header"
	"github.com/sirupsen/logrus"
)

// componentBackup holds the result of a single component snapshot creation.
type componentBackup struct {
	component string
	mac       objects.MAC
}

// CreatePlakarBackup creates an Infrahub backup as multiple Plakar snapshots (one per component),
// streaming database dumps directly from container exec stdout into kloset.
func (iops *InfrahubOps) CreatePlakarBackup(force bool, neo4jMetadata string, excludeTaskManager bool, sleepDuration time.Duration, redact bool) (retErr error) {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// The run's single exit path for every transient workload it creates
	// (FR-011). It runs after the streams have been read and closed, so the
	// per-stream releases have already given back what they could and this
	// removes only what a failure left standing.
	defer iops.releaseTransientWorkloads()

	// Where each database lives, before the edition probe reads a missing
	// container as Community and before anything is stopped for it.
	if err := iops.prepareDatabaseCapture(!excludeTaskManager); err != nil {
		return err
	}

	// Detect Neo4j edition. A probe that did not answer stops the run here,
	// before the abort window and before anything is stopped: the edition
	// decides whether Neo4j has to be taken offline, and a failed probe is not
	// evidence that it does.
	editionInfo := iops.detectNeo4jEditionInfo("backup")
	if err := editionInfo.requireDeterminedEdition(); err != nil {
		return err
	}
	if editionInfo.RequiresOfflineCapture() {
		logrus.Warn("Neo4j Community Edition detected; Infrahub services will be stopped and restarted before the backup begins.")
		logrus.Warn("Waiting 10 seconds to allow the user to abort... CTRL+C to cancel.")
		time.Sleep(10 * time.Second)
	}

	// Redact attribute values if requested
	if redact {
		if !force {
			return fmt.Errorf("--redact is a destructive operation that replaces all attribute values in the database with random UUIDs; use --force to confirm")
		}
		if err := iops.redactDatabase(); err != nil {
			return err
		}
	}

	version := iops.getInfrahubVersion()

	// Check for running tasks unless --force is set
	if !force {
		logrus.Info("Checking for running tasks before backup...")
		if err := iops.waitForRunningTasks(); err != nil {
			return err
		}
	}

	// Stop app containers for community edition
	var servicesToRestart []string
	if editionInfo.RequiresOfflineCapture() {
		stoppedServices, stopErr := iops.stopAppContainers()
		if stopErr != nil {
			// As in CreateBackup: whatever was taken down before the failure
			// goes back up, and what could not be is named (FR-013).
			iops.returnAppContainersToScale(stoppedServices)

			return fmt.Errorf("failed to stop services for Neo4j Community backup: %w", stopErr)
		}
		servicesToRestart = append([]string(nil), stoppedServices...)
		defer func() {
			if len(servicesToRestart) == 0 {
				return
			}
			if startErr := iops.startAppContainers(servicesToRestart); startErr != nil {
				logrus.Errorf("Failed to restart services after backup: %v", startErr)
			}
		}()

		// As in CreateBackup: asked for is not stopped, and the streaming
		// capture below reads the store offline. See
		// confirmAppContainersQuiesced for why a recorded replica count is not
		// a quiesced deployment.
		if err := iops.confirmAppContainersQuiesced(servicesToRestart); err != nil {
			return err
		}
	}

	// Generate backup-id timestamp
	backupID := time.Now().Format("20060102_150405")

	logrus.WithFields(logrus.Fields{
		"repo":          iops.config.Plakar.RepoPath,
		"neo4j_edition": editionInfo.Edition,
		"backup_id":     backupID,
	}).Info("Creating Plakar streaming backup")

	// Initialize Plakar context and repository
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return fmt.Errorf("failed to initialize plakar context: %w", err)
	}
	defer closePlakarContext(kctx)

	repo, err := openOrCreateRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	hostname := kctx.Hostname

	// Build component list
	components := []string{ComponentNeo4j}
	if !excludeTaskManager {
		components = append(components, ComponentPostgres)
	}
	components = append(components, ComponentMetadata)

	// Track completed snapshots for partial failure handling
	var completed []componentBackup

	// FR-012 on this backend: a run that does not produce every component
	// leaves no artefact behind. The components commit one at a time, so a
	// failure on the second one leaves the first committed and listable — and
	// `logIncompleteBackup` used to be the whole of the response to that, which
	// is a warning in a terminal nobody may be watching rather than the
	// removal the requirement asks for. It is registered after `defer
	// closeRepo(repo)` so it runs before the repository is closed.
	//
	// This is the same invariant the tarball path states as `reach`: written as
	// a deferred sweep over what was actually created, rather than as a branch
	// of each failure's own handling, because there are several ways out of the
	// loop below and every one of them may have committed a snapshot.
	defer func() {
		if retErr == nil && iops.capturesComplete() {
			return
		}

		if discardErr := iops.discardPlakarComponents(repo.DeleteSnapshot, completed, backupID); discardErr != nil {
			if retErr == nil {
				retErr = discardErr
			} else {
				retErr = fmt.Errorf("%w; %w", retErr, discardErr)
			}
		}
	}()

	// Generate backup metadata for the metadata component
	metadataObj := iops.createBackupMetadata(
		fmt.Sprintf("infrahub_backup_%s", backupID),
		!excludeTaskManager, version, editionInfo.Edition,
	)
	if redact {
		metadataObj.Redacted = true
	}
	// Override components to use Plakar naming (neo4j, postgres, metadata)
	// instead of the tarball naming (database, task-manager-db) from createBackupMetadata
	metadataObj.Components = components

	// Create one snapshot per component
	for _, component := range components {
		logrus.Infof("Creating snapshot for component: %s", component)

		var imp *StreamingImporter
		switch component {
		case ComponentNeo4j:
			dataFunc, err := iops.neo4jStreamFactory(editionInfo.Edition, neo4jMetadata)
			if err != nil {
				return fmt.Errorf("failed to prepare neo4j stream: %w", err)
			}
			now := time.Now()
			fi := objects.NewFileInfo("/neo4j-backup.tar", 0, 0644, now, 0, 0, 0, 0, 0)
			// Named for what the factory above actually streamed, which it
			// chose from Edition.
			if strings.EqualFold(editionInfo.Edition, neo4jEditionCommunity) {
				fi = objects.NewFileInfo("/neo4j.dump", 0, 0644, now, 0, 0, 0, 0, 0)
			}
			imp = NewStreamingImporter(hostname, fi.Name(), fi, dataFunc)

		case ComponentPostgres:
			dataFunc, err := iops.postgresStreamFactory()
			if err != nil {
				return fmt.Errorf("failed to prepare postgres stream: %w", err)
			}
			now := time.Now()
			fi := objects.NewFileInfo("/prefect.dump", 0, 0644, now, 0, 0, 0, 0, 0)
			imp = NewStreamingImporter(hostname, "/prefect.dump", fi, dataFunc)

		case ComponentMetadata:
			// FR-012's gate on this backend. The captures have run by the time
			// this component is reached, so this is the first moment the
			// completeness verdict is final — and the last moment at which
			// nothing has been offered as a restore point, because the metadata
			// component is created last and a group without it is not
			// restorable.
			//
			// What an incomplete verdict has to do is stop the run and take the
			// committed components with it, which is what happens here and in
			// the deferred discard — not describe itself in a snapshot that
			// would then be listable. So the gate stays ahead of the field.
			if !iops.capturesComplete() {
				return fmt.Errorf("the backup is incomplete and will not be kept (%s)", iops.incompleteCaptureDetail())
			}

			// The field is still written from here rather than from
			// createBackupMetadata, which runs before either capture: read
			// there it was a claim made before the evidence existed, and it
			// could only ever say `true`. Read here it reports what the
			// captures said, and cannot disagree with the gate above it
			// (see applyCaptureCompleteness).
			iops.applyCaptureCompleteness(metadataObj)

			metadataBytes, err := json.MarshalIndent(metadataObj, "", "    ")
			if err != nil {
				return fmt.Errorf("failed to marshal metadata: %w", err)
			}
			imp = NewMemoryImporter(hostname, "/backup_information.json", metadataBytes)
		}

		// Create snapshot
		tags := buildSnapshotTags(metadataObj, component, backupID, iops.snapshotCaptureStatus())
		builderOpts := &snapshot.BuilderOptions{
			Name: fmt.Sprintf("%s_%s", backupID, component),
			Tags: tags,
		}
		mac, err := iops.commitComponentSnapshot(repo, component, imp, builderOpts)
		if err != nil {
			return err
		}

		completed = append(completed, componentBackup{component: component, mac: mac})
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":  backupID,
		"components": len(completed),
		"repo":       iops.config.Plakar.RepoPath,
	}).Info("Plakar streaming backup completed successfully")

	// Sleep if requested
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil
}

// neo4jStreamFactory returns a data factory for streaming Neo4j backup data.
func (iops *InfrahubOps) neo4jStreamFactory(edition string, backupMetadata string) (func() (io.ReadCloser, error), error) {
	switch strings.ToLower(edition) {
	case neo4jEditionCommunity:
		return iops.backupNeo4jCommunityStream()
	default:
		return iops.backupNeo4jEnterpriseStream(backupMetadata)
	}
}

// postgresStreamFactory returns a data factory for streaming PostgreSQL backup data.
func (iops *InfrahubOps) postgresStreamFactory() (func() (io.ReadCloser, error), error) {
	return iops.backupTaskManagerDBStream()
}

// commitComponentSnapshot streams one component into a snapshot and commits it,
// or refuses to commit one that holds nothing worth restoring.
//
// Kloset's Backup and Commit both return nil when a record's reader fails: the
// failure is written into the snapshot as an error entry and counted in the
// source's summary, and the snapshot is committed with no bytes in it (pinned
// by TestKlosetCommitsASourceWhoseReaderFailed). On this path the reader is
// the dump the vendor tool streams, so a neo4j-admin or pg_dump that failed —
// wrong credentials, a full scratch disk, a listener that never answered — was
// a committed empty snapshot, "completed successfully" in the log, exit 0 and
// `capture_complete: true`, because recordIncompleteCapture is only reached
// from an error and there was none. That is the one outcome a backup tool must
// not have, so the summary is read before the commit: a source that recorded
// an error, or moved no bytes, is refused here, with nothing committed and so
// nothing to discard for this component. The components already committed go
// with the run, through the deferred sweep in CreatePlakarBackup.
//
// The verdict is recorded the way runExternalNeo4jCapture records its own, so
// that FR-012's other half is decided from what the capture said rather than
// from an error value a caller could replace, and so that the completeness
// field cannot read true for a run that reached this refusal.
func (iops *InfrahubOps) commitComponentSnapshot(repo *repository.Repository, component string, imp *StreamingImporter, opts *snapshot.BuilderOptions) (objects.MAC, error) {
	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, opts)
	if err != nil {
		return objects.NilMac, fmt.Errorf("failed to create snapshot for %s: %w", component, err)
	}
	defer builder.Close()

	source, err := snapshot.NewSource(context.Background(), 0, imp)
	if err != nil {
		return objects.NilMac, fmt.Errorf("failed to create source for %s: %w", component, err)
	}

	if err := builder.Backup(source); err != nil {
		return objects.NilMac, fmt.Errorf("streaming backup failed for %s: %w", component, err)
	}

	if refusal := summarizeSnapshotSources(builder.Header.Sources).refusal(); refusal != "" {
		if service, isCapture := captureServiceFor(component); isCapture {
			iops.recordIncompleteCapture(service, refusal)
		}

		return objects.NilMac, fmt.Errorf("the %s snapshot is incomplete and will not be kept: %s", component, refusal)
	}

	if err := builder.Commit(); err != nil {
		return objects.NilMac, fmt.Errorf("failed to commit snapshot for %s: %w", component, err)
	}

	mac := builder.Header.Identifier
	logrus.WithFields(logrus.Fields{
		"component":   component,
		"snapshot_id": fmt.Sprintf("%x", mac[:8]),
	}).Info("Component snapshot created")

	return mac, nil
}

// snapshotSourceVerdict is what a snapshot's own header says about the capture
// it holds: the bytes its sources stored and the errors they recorded, summed
// the way kloset's Commit sums them for its own report.
type snapshotSourceVerdict struct {
	Bytes  uint64
	Errors uint64
}

// summarizeSnapshotSources reads the verdict off the header Backup filled in.
// A header with no source at all reads as no bytes, which is the safe
// direction.
func summarizeSnapshotSources(sources []header.Source) snapshotSourceVerdict {
	var verdict snapshotSourceVerdict
	for _, source := range sources {
		verdict.Bytes += source.Summary.Directory.Size + source.Summary.Below.Size
		verdict.Errors += source.Summary.Directory.Errors + source.Summary.Below.Errors
	}

	return verdict
}

// refusal is why the snapshot is not one to commit, or "" when it is. No
// component this tool writes is legitimately empty — a dump, a backup archive
// and the metadata document all carry bytes — so no bytes is refused whatever
// the error count says.
func (v snapshotSourceVerdict) refusal() string {
	switch {
	case v.Errors > 0:
		return fmt.Sprintf("its source recorded %d error(s) while being read and %d byte(s) were stored, so the stream that fed it did not complete", v.Errors, v.Bytes)
	case v.Bytes == 0:
		return "its source recorded no error but no bytes were stored, so the stream that fed it produced nothing"
	}

	return ""
}

// captureServiceFor names the deployment service whose capture a component
// snapshot holds, for the record recordIncompleteCapture keeps per service.
// The metadata component is this tool's own document, not a capture, so it
// has no service: a refused metadata snapshot still fails the run, and the
// deferred sweep runs on the error alone.
func captureServiceFor(component string) (string, bool) {
	switch component {
	case ComponentNeo4j:
		return serviceNeo4j, true
	case ComponentPostgres:
		return serviceTaskManagerDB, true
	}

	return "", false
}

// discardPlakarComponents removes the component snapshots a run committed
// before it stopped, so that a run which did not produce a complete artefact
// leaves none behind (FR-012).
//
// It is the Plakar backend's discardIncompleteCapture, and it exists for the
// same reason: the requirement is not "fail", it is "fail and retain nothing".
// The tarball path deletes its archive from every location it reached; here the
// locations are snapshots in the repository, committed one component at a time,
// and the run that stopped between two of them had left the first listable.
//
// Removal rather than a status tag, for the reason the tarball path gives:
// determineGroupStatus does read the incomplete tag, but a group that exists
// still occupies a listing an operator reads and a rank a selection walks, and
// the tag on the component created *before* a capture ran cannot state that
// capture's outcome. A group that does not exist decides nothing.
//
// Every snapshot is attempted before it returns, and one that could not be
// removed is named rather than swallowed — that snapshot is then still in the
// repository, and its group is missing components, which is what
// determineGroupStatus refuses it on.
//
// remove is taken as a function rather than the repository itself, following
// the package's injection idiom (podRunner, transientClusterOps): the sweep is
// then assertable without a kloset repository on disk, and the production call
// site passes repository.DeleteSnapshot.
func (iops *InfrahubOps) discardPlakarComponents(remove snapshotRemover, completed []componentBackup, backupID string) error {
	if len(completed) == 0 {
		return nil
	}

	names := make([]string, len(completed))
	for i, c := range completed {
		names[i] = c.component
	}
	logrus.Warnf("Backup group %s did not complete; removing the %d component snapshot(s) it had already committed (%s)",
		backupID, len(completed), strings.Join(names, ", "))

	var problems []error
	for _, c := range completed {
		if err := remove(c.mac); err != nil {
			problems = append(problems, fmt.Errorf("component %s (snapshot %x): %w", c.component, c.mac[:8], err))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("the incomplete backup group %s may still be present in %s: %w",
		backupID, iops.plakarRepoPath(), errors.Join(problems...))
}

// snapshotRemover removes one snapshot from a repository, which is what
// repository.DeleteSnapshot does.
type snapshotRemover func(objects.MAC) error

// plakarRepoPath names the repository for a message, without assuming the
// backend was configured: a failure that has to name where an artefact was left
// must not itself be a nil dereference.
func (iops *InfrahubOps) plakarRepoPath() string {
	if iops.config == nil || iops.config.Plakar == nil {
		return "the plakar repository"
	}

	return iops.config.Plakar.RepoPath
}

// snapshotCaptureStatus is the status this run's snapshots are tagged with.
//
// It used to be the constant StatusComplete, which made the tag a statement
// about the tagging rather than about the capture: a run that recorded
// `capture_complete: false` in backup_information.json still tagged every
// snapshot `complete`, and the group status the restore-point selection ranks
// on had nothing else to read. FR-012 requires an incomplete capture not to be
// offered as a restore point, so the tag now says what the run knows at the
// moment the snapshot is created.
//
// It is read per component rather than once, because the verdict only becomes
// final as the captures run: the Neo4j snapshot is created before its own
// stream has moved a byte, so it cannot carry its own verdict. What makes that
// sufficient is the component order — the metadata snapshot is created last and
// every group has one — so a run with any incomplete capture always produces at
// least one snapshot tagged incomplete, which is what determineGroupStatus
// reads.
func (iops *InfrahubOps) snapshotCaptureStatus() string {
	if iops.capturesComplete() {
		return StatusComplete
	}

	return StatusIncomplete
}

// buildSnapshotTags creates Plakar snapshot tags for a component snapshot.
func buildSnapshotTags(metadata *BackupMetadata, component, backupID, status string) []string {
	tags := []string{
		TagBackupID + "=" + backupID,
		TagComponent + "=" + component,
		TagBackupStatus + "=" + status,
		TagVersion + "=" + metadata.InfrahubVersion,
		TagToolVersion + "=" + metadata.ToolVersion,
		TagNeo4jEdition + "=" + metadata.Neo4jEdition,
		TagComponents + "=" + strings.Join(metadata.Components, ","),
	}
	if metadata.Redacted {
		tags = append(tags, TagRedacted+"=true")
	}
	return tags
}
