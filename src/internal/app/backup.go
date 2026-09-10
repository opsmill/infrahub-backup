package app

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// retentionLegsForCreate returns the storage locations a successful `create` run
// prunes, and is the gate that decides whether retention runs at all.
//
// An inactive policy yields no legs, so a run without retention options behaves
// exactly as it did before this feature existed (FR-004). When the policy is
// active the local backup directory is always a leg, and S3 is a leg exactly when
// this run uploaded there — the mere presence of S3 configuration never causes an
// object to be deleted (FR-007).
func retentionLegsForCreate(cfg *Configuration, s3UploadedThisRun bool) ([]storageLocation, error) {
	if !cfg.Retention.Policy().Active() {
		return nil, nil
	}

	legs := []storageLocation{newLocalLocation(cfg.BackupDir)}
	if !s3UploadedThisRun {
		return legs, nil
	}

	client, err := NewS3Client(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client for retention: %w", err)
	}

	return append(legs, newS3Location(client)), nil
}

// warnRetentionUnsupportedBackend reports that an active retention policy will not
// be applied because the selected backend has no retention implementation yet.
// Warning instead of failing keeps the backup itself working, and warning at all
// keeps the combination from being a silent no-op (FR-012).
func warnRetentionUnsupportedBackend(cfg *Configuration) {
	if !cfg.Retention.Policy().Active() {
		return
	}

	logrus.Warnf("retention not yet supported for the %s backend; skipping retention (no backups will be pruned)", cfg.Backend)
}

// applyCreateRetention applies the configured retention policy after a backup has
// fully succeeded. It reports the joined per-leg failures; every leg is attempted
// before it returns (FR-008).
func (iops *InfrahubOps) applyCreateRetention(s3UploadedThisRun bool) error {
	legs, err := retentionLegsForCreate(iops.config, s3UploadedThisRun)
	if err != nil {
		return err
	}
	if len(legs) == 0 {
		return nil
	}

	policy := iops.config.Retention.Policy()
	logrus.Infof("Applying retention policy (days: %d, count: %d) to %d location(s)", policy.Days, policy.Count, len(legs))

	_, err = applyRetention(context.Background(), legs, policy, retentionExecute)

	return err
}

// ---------------------------------------------------------------------------
// An incomplete capture is not retained (FR-012)
// ---------------------------------------------------------------------------

// recordIncompleteCapture remembers that one database's capture was not a
// complete one, with the account the operator will read.
//
// The capture path fails the run at the same moment. This record exists because
// failing is only half of FR-012: the other half is that no artefact survives
// the run, and that has to be decided from the verdict itself rather than from
// an error value, which any layer between the capture and the archive could
// wrap, replace or — on a path that only logs — drop.
func (iops *InfrahubOps) recordIncompleteCapture(service, detail string) {
	if iops.incompleteCaptures == nil {
		iops.incompleteCaptures = map[string]string{}
	}

	iops.incompleteCaptures[service] = detail
}

// capturesComplete reports whether every capture this run made reported a
// complete capture. It is true for a run that captured nothing external, which
// is every internal deployment.
func (iops *InfrahubOps) capturesComplete() bool {
	return len(iops.incompleteCaptures) == 0
}

// incompleteCaptureDetail renders why the run's captures were not complete, in a
// stable order so two runs against the same fault say the same thing.
func (iops *InfrahubOps) incompleteCaptureDetail() string {
	services := make([]string, 0, len(iops.incompleteCaptures))
	for service := range iops.incompleteCaptures {
		services = append(services, service)
	}
	slices.Sort(services)

	details := make([]string, 0, len(services))
	for _, service := range services {
		details = append(details, service+": "+iops.incompleteCaptures[service])
	}

	return strings.Join(details, "; ")
}

// artifactReach is where this run's archive actually landed. It is the input to
// FR-012's removal, which has to cover every location the artefact reached
// rather than only the last one written to: with --s3-upload the object is in
// the bucket before retention or a restore-point listing would ever consider it,
// and one left there is one a later run can select.
type artifactReach struct {
	// localNames are the names the archive has had in the backup directory, in
	// the order it took them. There is more than one because encrypting renames
	// it, and removing the plaintext afterwards is a warning rather than a
	// failure — so a run can legitimately leave both behind, and both are
	// archives retention would count.
	//
	// Names are not cleared when the run removes a file itself: deleting
	// something already gone is the outcome the deletion asked for, and clearing
	// them would make the removal depend on which flags the run was given rather
	// than on where the artefact went.
	localNames []string

	// s3Name is the name that reached the bucket, empty until the upload
	// returned. Configuration alone never puts an object in a bucket, so it
	// never authorises a deletion from one — the same rule
	// retentionLegsForCreate follows for pruning.
	s3Name string
}

// reachedLocally records a name the archive now has in the backup directory.
func (r *artifactReach) reachedLocally(name string) {
	if name == "" || slices.Contains(r.localNames, name) {
		return
	}

	r.localNames = append(r.localNames, name)
}

// removeArtifactFrom deletes one named archive from one location, through the
// same storageLocation deletion retention uses: the name is re-checked against
// the archive pattern before anything is removed, and an archive already gone is
// not a failure.
func removeArtifactFrom(ctx context.Context, leg storageLocation, name string) error {
	ref, ok := parseBackupName(name)
	if !ok {
		// Delete would refuse this name anyway. Saying so here is the difference
		// between an operator knowing an artefact may still be out there and
		// believing the run cleaned up after itself.
		return fmt.Errorf("cannot remove %q from %s: it is not a backup archive name", name, leg.Name())
	}

	if err := leg.Delete(ctx, ref); err != nil {
		return fmt.Errorf("failed to remove the incomplete backup from %s: %w", leg.Name(), err)
	}

	logrus.Infof("Removed the incomplete backup %s from %s", name, leg.Name())

	return nil
}

// discardIncompleteCapture is the rest of FR-012: a capture that did not capture
// everything it was asked for must not be retained, so the run fails and the
// artefact is removed from every location it reached.
//
// Removal rather than a metadata flag is what keeps retention out of this.
// Retention selects from artefact names and recency ranks and never reads
// metadata, so an artefact merely *marked* incomplete would still occupy a keep
// slot and evict a good backup — and a previously released tool, which ignores
// fields it has never heard of, would have offered it as a restore point. An
// artefact that does not exist changes nothing either of them decides.
//
// Removal is not a prune, so unlike retentionLegsForCreate this consults no
// retention policy: a deployment that never configured retention is not thereby
// asking to keep an unusable backup.
//
// It returns the failure the run reports. Every location is attempted before it
// returns, and one that could not be cleared is named rather than swallowed —
// the artefact is then still out there, and BackupMetadata.CaptureComplete is
// what remains to identify it.
func (iops *InfrahubOps) discardIncompleteCapture(reach artifactReach) error {
	failure := fmt.Errorf("the backup is incomplete and will not be kept (%s)", iops.incompleteCaptureDetail())

	ctx := context.Background()
	var problems []error

	if len(reach.localNames) > 0 {
		local := newLocalLocation(iops.config.BackupDir)
		for _, name := range reach.localNames {
			if err := removeArtifactFrom(ctx, local, name); err != nil {
				problems = append(problems, err)
			}
		}
	}

	if reach.s3Name != "" {
		client, err := NewS3Client(iops.config.S3)
		if err != nil {
			problems = append(problems, fmt.Errorf("failed to create S3 client to remove the incomplete backup: %w", err))
		} else if err := removeArtifactFrom(ctx, newS3Location(client), reach.s3Name); err != nil {
			problems = append(problems, err)
		}
	}

	if len(problems) == 0 {
		return failure
	}

	return fmt.Errorf("%w; it may still be present: %w", failure, errors.Join(problems...))
}

// loadEncryptionKey loads the public key for encryption.
// If keyPath is empty, returns the default hardcoded key.
func loadEncryptionKey(keyPath string) (*ecdh.PublicKey, error) {
	if keyPath != "" {
		return LoadPublicKeyFromFile(keyPath)
	}
	return DefaultPublicKey()
}

// CreateBackup creates a full backup of the Infrahub deployment
func (iops *InfrahubOps) CreateBackup(force bool, neo4jMetadata string, excludeTaskManager bool, s3Upload bool, s3KeepLocal bool, sleepDuration time.Duration, redact bool, encrypt bool, encryptKey string) (retErr error) {
	if iops.config.Backend == BackendPlakar {
		// The Plakar backend has no retention implementation yet. Warn up front so
		// the operator cannot mistake the run for one that pruned, then run the
		// backup normally and prune nothing (FR-012).
		warnRetentionUnsupportedBackend(iops.config)
		return iops.CreatePlakarBackup(force, neo4jMetadata, excludeTaskManager, sleepDuration, redact)
	}

	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// The run's single exit path for every transient workload it creates
	// (FR-011). Deferred before the gate below rather than after it, because the
	// gate is what creates the first one — a probe that answered and then hit a
	// version refusal has already put a pod in the namespace.
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

	var servicesToRestart []string
	if editionInfo.RequiresOfflineCapture() {
		stoppedServices, stopErr := iops.stopAppContainers()
		if stopErr != nil {
			// Whatever was taken down before the failure goes back up, and
			// what could not be is named (FR-013).
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
				if retErr == nil {
					retErr = fmt.Errorf("failed to restart services after backup: %w", startErr)
				}
			}
		}()

		// Asked for is not stopped, and the capture below reads the store
		// offline. This is the same confirmation the restore paths make before
		// their own destructive step, and it is needed here for the reason
		// stopAppContainers states: `kubectl scale` returns as soon as the
		// replica count is recorded, so an infrahub-server still holding a Bolt
		// session is a writer attached to the store `neo4j-admin dump` is about
		// to read — a torn dump reported as a successful backup.
		//
		// After the restart defer, so a deployment that will not quiesce is
		// returned to the scale it was found at rather than left down.
		if err := iops.confirmAppContainersQuiesced(servicesToRestart); err != nil {
			return err
		}
	}

	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)

	// Where the archive has got to, updated as it gets there, so FR-012's
	// removal covers every location rather than the one the run last touched.
	var reach artifactReach

	// FR-012's backstop, registered before the captures run so that no path out
	// of them can skip it: if a capture that was not complete somehow reached an
	// archive, the archive does not survive the run. It is written as an
	// invariant rather than as a branch of the capture's own error handling,
	// because the verdict — not an error value some layer between could wrap,
	// replace or drop — is what decides whether an artefact may be kept.
	//
	// On the ordinary path the gate after the captures has already handled it
	// and cleared reach, so this does nothing.
	defer func() {
		if iops.capturesComplete() || (len(reach.localNames) == 0 && reach.s3Name == "") {
			return
		}

		if retErr != nil {
			logrus.Errorf("The backup failed: %v", retErr)
		}

		retErr = iops.discardIncompleteCapture(reach)
	}()

	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	logrus.WithFields(logrus.Fields{
		"filename":      backupFilename,
		"backup_dir":    iops.config.BackupDir,
		"neo4j_edition": editionInfo.Edition,
	}).Info("Creating backup")

	// Create backup directory structure
	backupDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup parent directory: %w", err)
	}

	// Create metadata
	backupID := strings.TrimSuffix(backupFilename, ".tar.gz")
	metadata := iops.createBackupMetadata(backupID, !excludeTaskManager, version, editionInfo.Edition)
	if redact {
		metadata.Redacted = true
	}
	if encrypt || encryptKey != "" {
		metadata.Encrypted = true
	}

	// Backup databases
	if err := iops.backupDatabase(backupDir, neo4jMetadata, editionInfo.Edition); err != nil {
		return err
	}

	if !excludeTaskManager {
		if err := iops.backupTaskManagerDB(backupDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database backup as requested")
	}

	// FR-012's gate. This is the last moment the completeness verdict can
	// change — only the capture paths above record one — and nothing downstream
	// may advance an artefact this run must not keep.
	//
	// It matters that the check is here rather than only in the deferred
	// backstop. Retention ranks by recency, so an archive written and then
	// removed would still have been the location's newest while the prune ran,
	// and would have pushed the real newest down a rank: exactly the eviction
	// FR-012 exists to prevent.
	if !iops.capturesComplete() {
		discardErr := iops.discardIncompleteCapture(reach)
		reach = artifactReach{}

		return discardErr
	}

	// Calculate checksums for backup files
	checksums, err := calculateBackupChecksums(backupDir, excludeTaskManager)
	if err != nil {
		return err
	}
	metadata.Checksums = checksums

	// What the captures reported, read now that they have run rather than when
	// the metadata was built above — which was before backupDatabase (FR-012,
	// see applyCaptureCompleteness).
	iops.applyCaptureCompleteness(metadata)

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// TODO: Backup artifact store
	logrus.Info("Artifact store backup will be added in future versions")

	// Create tarball
	logrus.Info("Creating backup archive...")
	if err := createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	reach.reachedLocally(backupFilename)

	// Encrypt backup if requested
	if encrypt || encryptKey != "" {
		pubKey, loadErr := loadEncryptionKey(encryptKey)
		if loadErr != nil {
			return fmt.Errorf("failed to load encryption key: %w", loadErr)
		}

		encryptedPath := backupPath + ".enc"
		logrus.Info("Encrypting backup archive...")
		if err := EncryptFile(backupPath, encryptedPath, pubKey); err != nil {
			return fmt.Errorf("failed to encrypt backup: %w", err)
		}

		if err := os.Remove(backupPath); err != nil {
			logrus.Warnf("Failed to remove plaintext backup: %v", err)
		}

		backupPath = encryptedPath
		backupFilename = filepath.Base(encryptedPath)
		reach.reachedLocally(backupFilename)
	}

	// Log backup creation with structured fields
	fields := logrus.Fields{
		"path":     backupPath,
		"filename": backupFilename,
	}
	if stat, err := os.Stat(backupPath); err == nil {
		fields["size_bytes"] = stat.Size()
		fields["size_human"] = formatBytes(stat.Size())
	}
	logrus.WithFields(fields).Info("Backup created successfully")

	// Upload to S3 if requested
	if s3Upload {
		s3URI, err := iops.uploadBackupToS3(backupPath)
		if err != nil {
			return fmt.Errorf("backup created locally but S3 upload failed: %w", err)
		}
		reach.s3Name = backupFilename
		logrus.Infof("Backup uploaded to: %s", s3URI)

		if !s3KeepLocal {
			if err := os.Remove(backupPath); err != nil {
				logrus.Warnf("Failed to delete local backup file: %v", err)
			} else {
				logrus.Infof("Local backup file deleted: %s", backupPath)
			}
		}
	}

	// Apply retention only now: the archive is written, checksummed, and — when an
	// upload was requested — safely in S3, so a failed backup can never trigger a
	// deletion. Each leg's keep-newest floor protects that location's newest
	// archive, which is this run's at every location still holding it; with
	// --s3-upload and without --s3-keep-local the local copy was removed just
	// above, so the local leg's newest is the previous archive instead. The prune
	// runs before the transfer sleep so an operator who interrupts the sleep does
	// not skip it.
	if err := iops.applyCreateRetention(s3Upload); err != nil {
		return fmt.Errorf("backup succeeded (%s); retention failed: %w", backupFilename, err)
	}

	// Sleep if requested (for K8s users to transfer backup file)
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		logrus.Info("Press Ctrl+C to exit early if file transfer is complete")
		time.Sleep(sleepDuration)
	}

	return retErr
}

// RestoreBackup restores an Infrahub deployment from a backup archive
func (iops *InfrahubOps) RestoreBackup(backupFile string, excludeTaskManager bool, restoreMigrateFormat bool, sleepDuration time.Duration, decryptKey string, force bool, resetDeploymentID bool) error {
	if iops.config.Backend == BackendPlakar {
		return iops.RestorePlakarBackup(excludeTaskManager, restoreMigrateFormat, sleepDuration, force, resetDeploymentID)
	}

	actualBackupFile := backupFile

	// An `s3://bucket/key` argument names an object that has to be on this host before it
	// can be restored. It is downloaded under a reserved temporary name rather than its own
	// — so it cannot land on a local archive of the same name — and removed again whatever
	// the restore's outcome. See restore_inputs.go.
	if IsS3URI(backupFile) {
		downloadedPath, err := iops.downloadBackupFromS3(backupFile)
		if err != nil {
			return err
		}
		actualBackupFile = downloadedPath
		defer removeRestoreTempPath(downloadedPath)
	}

	// Sleep if requested (for K8s users to transfer backup file into pod)
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		logrus.Info("Press Ctrl+C to cancel if you need more time")
		time.Sleep(sleepDuration)
	}

	if _, err := os.Stat(actualBackupFile); os.IsNotExist(err) {
		return fmt.Errorf("backup file not found: %s", actualBackupFile)
	}

	// Auto-detect and decrypt if necessary
	encrypted, err := IsEncryptedFile(actualBackupFile)
	if err != nil {
		return fmt.Errorf("failed to detect file format: %w", err)
	}

	if encrypted {
		if decryptKey == "" {
			return fmt.Errorf("backup file is encrypted; provide --decrypt-key to decrypt")
		}

		privKey, err := LoadPrivateKeyFromFile(decryptKey)
		if err != nil {
			return fmt.Errorf("failed to load decryption key: %w", err)
		}

		// Decrypt through a reserved temporary name beside the archive, never to the
		// archive's own name minus ".enc": that name is a valid archive name in the same
		// pool, so a plain archive of the same timestamp sitting next to the encrypted one
		// was overwritten by the plaintext and then deleted with it when the restore
		// finished — two archives in, one out, and a run that reported success. The
		// `--latest` tiebreak deliberately prefers the .enc member of such a pair, so an
		// operator could reach this without ever naming the encrypted archive.
		// See restore_inputs.go for the convention this follows.
		decryptedPath, err := reserveRestoreTempPath(filepath.Dir(actualBackupFile), decryptRestoreTempPattern)
		if err != nil {
			return fmt.Errorf("failed to prepare the decryption of %s: %w", actualBackupFile, err)
		}
		defer removeRestoreTempPath(decryptedPath)

		logrus.Info("Decrypting backup archive...")
		if err := DecryptFile(actualBackupFile, decryptedPath, privKey); err != nil {
			return fmt.Errorf("failed to decrypt backup: %w", err)
		}

		// A downloaded encrypted archive has no further use once its plaintext exists, so it
		// goes now rather than at the end of the run: holding both is twice the archive's
		// size on a disk that only ever had to hold one.
		if IsS3URI(backupFile) {
			removeRestoreTempPath(actualBackupFile)
		}

		actualBackupFile = decryptedPath
	} else if decryptKey != "" {
		return fmt.Errorf("--decrypt-key provided but backup file is not encrypted")
	}

	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Where each database lives, before the edition is misdiagnosed, before
	// transient data is wiped and before anything is scaled to zero. The
	// discovery this gate's successor needs reads `env` out of infrahub-server
	// and task-manager, so it cannot run any later than this: stopAppContainers
	// has scaled both to zero by then.
	//
	// It is also where a database outside the deployment gets the workload the
	// restore runs in, so the workloads this run created are given back on
	// every exit path from here on (FR-011). An all-internal deployment created
	// none and the call does nothing.
	defer iops.releaseTransientWorkloads()

	if err := iops.prepareDatabaseRestore(!excludeTaskManager); err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "infrahub_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	logrus.WithFields(logrus.Fields{
		"backup_file": backupFile,
		"work_dir":    workDir,
	}).Info("Starting backup restore")

	// Extract backup
	logrus.Info("Extracting backup archive...")
	if err := extractTarball(actualBackupFile, workDir); err != nil {
		return fmt.Errorf("failed to extract backup: %w", err)
	}

	// Validate backup
	metadataPath := filepath.Join(workDir, "backup", "backup_information.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return fmt.Errorf("invalid backup file: missing metadata")
	}

	// Read and parse backup info
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("failed to read metadata: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("failed to parse metadata: %w", err)
	}

	// Log backup metadata with structured fields
	logrus.WithFields(logrus.Fields{
		"backup_id":        metadata.BackupID,
		"created_at":       metadata.CreatedAt,
		"tool_version":     metadata.ToolVersion,
		"infrahub_version": metadata.InfrahubVersion,
		"neo4j_edition":    metadata.Neo4jEdition,
		"components":       metadata.Components,
	}).Info("Backup metadata loaded")

	// Before the checksums, the edition resolution and everything destructive:
	// an artefact whose own capture reported itself incomplete is not a restore
	// point (FR-012). It is also the refusal `--latest` needs, since selection
	// there has only the archive's name to go on and would pick exactly this
	// artefact for being the newest.
	if err := metadata.refuseIncompleteCapture(backupFile); err != nil {
		return err
	}

	// Detect Neo4j edition for restore
	detectedEdition, detectionErr := iops.detectNeo4jEdition()
	editionInfo := NewNeo4jEditionInfo(detectedEdition, detectionErr)

	neo4jEdition, err := editionInfo.ResolveRestoreEdition(metadata.Neo4jEdition)
	if err != nil {
		return err
	}
	editionInfo.LogDetection("restore")

	// Determine task manager database availability
	taskManagerIncluded := slices.Contains(metadata.Components, "task-manager-db")
	if !taskManagerIncluded {
		if _, ok := metadata.Checksums["prefect.dump"]; ok {
			taskManagerIncluded = true
		}
	}

	// Validate checksums for all backup files
	if err := validateBackupChecksums(workDir, &metadata, excludeTaskManager); err != nil {
		return err
	}

	// Determine if we should restore task manager database
	shouldRestoreTaskManager := taskManagerIncluded && !excludeTaskManager
	prefectPath := filepath.Join(workDir, "backup", prefectDumpFilename)
	prefectExists := fileExists(prefectPath)
	validatePrefect := shouldRestoreTaskManager && prefectExists

	// Validate task manager restore requirements
	if taskManagerIncluded && !prefectExists && !excludeTaskManager {
		return fmt.Errorf("backup metadata includes task manager database but %s is missing", prefectDumpFilename)
	}

	// Log task manager restore status
	if taskManagerIncluded && excludeTaskManager {
		logrus.Info("Skipping task manager database restore as requested")
	} else if !taskManagerIncluded {
		logrus.Info("Backup does not include task manager database; skipping restore")
	} else if prefectExists {
		logrus.Info("Task manager database dump detected; will restore")
	}

	// The last check that can be made with the deployment still up and the
	// databases still holding their data: for a database outside the
	// deployment, the server fetches the artifact itself, so the artifact is
	// staged where it can be read from, and a run that cannot establish that
	// stops here (FR-007). It does nothing for an in-deployment database.
	if err := iops.preflightExternalNeo4jRestore(workDir); err != nil {
		return err
	}

	// Wipe transient data
	iops.wipeTransientData()

	// Stop application containers
	stopped, err := iops.stopAppContainers()
	if err != nil {
		// Whatever was taken down before the failure goes back up: the run is
		// over, and leaving a partially quiesced deployment behind is the
		// outage FR-013 exists to prevent.
		iops.returnAppContainersToScale(stopped)

		return err
	}

	// Every exit path from here to the ordinary restart below is a failure, and
	// each of them returns the deployment to the scale it was found at (FR-013).
	// The success path hands the list over by clearing it, so nothing is started
	// twice.
	defer func() {
		iops.returnAppContainersToScale(stopped)
	}()

	// Asked for is not stopped, and the next steps overwrite databases
	// (FR-026).
	if err := iops.confirmAppContainersQuiesced(stopped); err != nil {
		return err
	}

	// Restore PostgreSQL when available
	if validatePrefect {
		if err := iops.restorePostgreSQL(workDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database restore step")
	}

	// Restart dependencies
	if err := iops.restartDependencies(); err != nil {
		return err
	}

	// Restore Neo4j
	if err := iops.restoreNeo4j(workDir, neo4jEdition, restoreMigrateFormat); err != nil {
		return err
	}

	// Reset deployment ID before the app containers come back up so they never
	// observe the source deployment's UUID.
	if resetDeploymentID {
		if err := iops.resetDeploymentID(); err != nil {
			return err
		}
	}

	// Restart all services
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	// The deployment is back, so the deferred failure-path restart has nothing
	// left to do. What it would still be useful for is saying whether the
	// services actually came back up, which is what this reports (FR-026).
	iops.reportAppContainersRunning(stopped)
	stopped = nil

	logrus.Info("Restore completed successfully")
	logrus.Info("Infrahub should be available shortly")

	return nil
}

// CreateBackupFromFiles creates a backup archive from local Neo4j backup files and PostgreSQL dump.
// This is useful when you already have database dumps on the local filesystem and want to
// create a compatible backup archive without connecting to a running Infrahub instance.
func (iops *InfrahubOps) CreateBackupFromFiles(neo4jPath string, postgresPath string, neo4jEdition string, infrahubVersion string, encrypt bool, encryptKey string) error {
	// Validate input paths
	if neo4jPath == "" {
		return fmt.Errorf("neo4j backup path is required")
	}

	neo4jInfo, err := os.Stat(neo4jPath)
	if err != nil {
		return fmt.Errorf("neo4j backup path not accessible: %w", err)
	}

	var postgresIncluded bool
	if postgresPath != "" {
		if _, err := os.Stat(postgresPath); err != nil {
			return fmt.Errorf("postgres dump file not accessible: %w", err)
		}
		postgresIncluded = true
	}

	// Create work directory
	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	// Create backup directory structure
	backupDir := filepath.Join(workDir, "backup")
	databaseDir := filepath.Join(backupDir, "database")
	if err := os.MkdirAll(databaseDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup parent directory: %w", err)
	}

	logrus.Info("Copying Neo4j backup files...")

	// Copy Neo4j backup files
	if neo4jInfo.IsDir() {
		// Copy directory contents
		if err := copyDir(neo4jPath, databaseDir); err != nil {
			return fmt.Errorf("failed to copy neo4j backup directory: %w", err)
		}
	} else {
		// Copy single file (e.g., .dump file for community edition)
		destPath := filepath.Join(databaseDir, filepath.Base(neo4jPath))
		if err := copyFile(neo4jPath, destPath); err != nil {
			return fmt.Errorf("failed to copy neo4j backup file: %w", err)
		}
	}

	// Copy PostgreSQL dump if provided
	if postgresIncluded {
		logrus.Info("Copying PostgreSQL dump file...")
		destPath := filepath.Join(backupDir, "prefect.dump")
		if err := copyFile(postgresPath, destPath); err != nil {
			return fmt.Errorf("failed to copy postgres dump: %w", err)
		}
	}

	// Calculate checksums
	checksums := make(map[string]string)

	err = filepath.Walk(databaseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(backupDir, path)
			if sum, err := calculateSHA256(path); err == nil {
				checksums[rel] = sum
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate Neo4j backup checksums: %w", err)
	}

	if postgresIncluded {
		prefectPath := filepath.Join(backupDir, "prefect.dump")
		if sum, err := calculateSHA256(prefectPath); err == nil {
			checksums["prefect.dump"] = sum
		} else {
			return fmt.Errorf("failed to calculate Prefect DB checksum: %w", err)
		}
	}

	// Generate backup filename and ID
	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)
	backupID := strings.TrimSuffix(backupFilename, ".tar.gz")

	// Normalize neo4j edition
	edition := strings.ToLower(neo4jEdition)
	if edition == "" {
		// Try to detect from file structure
		// If it's a .dump file, likely community edition
		if !neo4jInfo.IsDir() && strings.HasSuffix(neo4jPath, ".dump") {
			edition = neo4jEditionCommunity
		} else {
			edition = neo4jEditionEnterprise
		}
		logrus.Infof("Auto-detected Neo4j edition: %s", edition)
	}

	// Create metadata
	metadata := iops.createBackupMetadata(backupID, postgresIncluded, infrahubVersion, edition)
	metadata.Checksums = checksums
	if encrypt || encryptKey != "" {
		metadata.Encrypted = true
	}

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Create tarball
	logrus.Info("Creating backup archive...")
	if err := createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	// Encrypt backup if requested
	if encrypt || encryptKey != "" {
		pubKey, loadErr := loadEncryptionKey(encryptKey)
		if loadErr != nil {
			return fmt.Errorf("failed to load encryption key: %w", loadErr)
		}

		encryptedPath := backupPath + ".enc"
		logrus.Info("Encrypting backup archive...")
		if err := EncryptFile(backupPath, encryptedPath, pubKey); err != nil {
			return fmt.Errorf("failed to encrypt backup: %w", err)
		}

		if err := os.Remove(backupPath); err != nil {
			logrus.Warnf("Failed to remove plaintext backup: %v", err)
		}

		backupPath = encryptedPath
	}

	logrus.Infof("Backup created: %s", backupPath)

	// Show backup size
	if stat, err := os.Stat(backupPath); err == nil {
		logrus.Infof("Backup size: %s", formatBytes(stat.Size()))
	}

	return nil
}

// copyFile copies a single file from src to dst
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// copyDir recursively copies a directory from src to dst
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}
