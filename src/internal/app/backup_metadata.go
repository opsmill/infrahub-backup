package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const metadataVersion = 2025111200

const (
	neo4jEditionEnterprise = "enterprise"
	neo4jEditionCommunity  = "community"
)

// isCommunityEdition reads an edition string the way every gate on the
// Community mechanism does: case-insensitively, with whatever padding a server
// or a metadata file put around it. An empty string is not Community — the
// asymmetry Neo4jEditionInfo documents — so a probe that did not answer never
// selects the destructive branch.
func isCommunityEdition(edition string) bool {
	return strings.EqualFold(strings.TrimSpace(edition), neo4jEditionCommunity)
}

// BackupMetadata represents the backup metadata structure
type BackupMetadata struct {
	MetadataVersion int               `json:"metadata_version"`
	BackupID        string            `json:"backup_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Components      []string          `json:"components"`
	Checksums       map[string]string `json:"checksums,omitempty"`
	Neo4jEdition    string            `json:"neo4j_edition,omitempty"`
	Redacted        bool              `json:"redacted,omitempty"`
	Encrypted       bool              `json:"encrypted,omitempty"`

	// The four fields below record where a capture's data came from when a
	// database lives outside the deployment (FR-005). They are additive and
	// metadataVersion does not move for them (FR-024): a reader that sees none
	// of them holds an internal, complete capture from an unrecorded endpoint,
	// which is exactly what every previously released version wrote — no
	// migration, no warning, no operator action.
	//
	// They are keyed by the deployment service name — serviceNeo4j and
	// serviceTaskManagerDB — rather than by whatever a backend calls its
	// components, because that is the name the endpoint was resolved under and
	// the Plakar backend renames its components afterwards. A run that read at
	// least one external database records every database it captured, including
	// the internal ones, so the presence of a key never says where that database
	// lives; ExternalLocation says it.

	// SourceEndpoints is what the run supplied for each database, in the order
	// the capture would try them. It is not a claim that the first entry served
	// the capture, and no field here claims that of any entry: with several
	// endpoints supplied, no documented interface reports which one answered.
	//
	// The port is the one the capture actually travelled over — see
	// DatabaseEndpoint.captureTargets, which is where the two services diverge.
	SourceEndpoints map[string][]string `json:"source_endpoints,omitempty"`

	// SourceRoles maps each supplied endpoint to the role observed for it at
	// capture time. `unknown` is a real value: a member the server named without
	// a role, or a role query that failed, records unknown, and a reader knows
	// the run looked. A database whose roles were never sought is absent from
	// this map instead — collapsing the two would claim an observation the run
	// never made.
	SourceRoles map[string]map[string]string `json:"source_roles,omitempty"`

	// CaptureComplete is what the capture *reported*, not what its exit status
	// implied — a backup that reached only some of several endpoints exits the
	// same way as one that failed outright (research R6).
	//
	// It is a pointer because absent means complete: a pre-feature artefact was
	// never partial by this definition. A plain bool with omitempty would encode
	// false as absent and so read back as its own opposite, which is the one
	// mistake this field cannot survive.
	//
	// It is written where the metadata is serialized rather than where it is
	// built, because the captures run in between: written at build time it was
	// a prediction of what they would say, and necessarily `true`. See
	// applyCaptureCompleteness.
	//
	// A reader should never see false, because FR-012 removes an incomplete
	// capture rather than retaining it. The field is what remains if that
	// removal itself fails: the artefact is still identifiable as unusable, and
	// a reader that does encounter it must not offer it as a restore point.
	CaptureComplete *bool `json:"capture_complete,omitempty"`

	// ExternalLocation is whether each captured database lived outside the
	// deployment. Diagnostic: it is what explains an artefact whose source
	// endpoint is not one of the deployment's own.
	ExternalLocation map[string]bool `json:"external_location,omitempty"`
}

// refuseIncompleteCapture is CaptureComplete's reader, and the reason the field
// is worth writing.
//
// FR-012 removes an incomplete capture rather than retaining one, so this
// should never fire. It fires when that removal failed — the archive could not
// be deleted, the object could not be removed from the bucket, a snapshot could
// not be deleted from the repository — each of which is reported as a failure
// and leaves an artefact behind that the *name* does not distinguish from a good
// one. Retention ranks by recency and never reads metadata, and `--latest`
// selects by name, so both will hand exactly that artefact to a restore, being
// the newest. This is the last point at which it can be told apart, and the
// contract's rule for a reader that meets it is categorical: it must not be
// offered as a restore point.
//
// There is deliberately no --force escape, which is the one place this is
// stricter than the incomplete-*group* refusal on the Plakar path. That refusal
// is about which components are present, a shortfall an operator can see and
// reason about — no task-manager dump, restore the rest. This one is about a
// capture that reported it had not finished reading the database, so what is in
// the artefact is unknown, and there is nothing for a bypass to be a judgement
// about. An operator whose only artefact says this needs the previous one.
//
// Absent means complete, which is precisely what every previously released
// version produced (FR-024).
func (m *BackupMetadata) refuseIncompleteCapture(source string) error {
	if m.CaptureComplete == nil || *m.CaptureComplete {
		return nil
	}

	return fmt.Errorf("refusing to restore %s: its metadata records capture_complete=false, meaning the capture that produced it did not finish reading every database it was asked for, so its contents are not a recovery point; an artefact this run had produced would have been removed rather than kept, so this one survived a failed removal — restore an earlier backup instead", source)
}

// errNeo4jEditionUndetermined is the refusal a backup gives when the edition
// probe did not answer. Callers that need to recognise it match it with
// errors.Is.
var errNeo4jEditionUndetermined = errors.New("the Neo4j edition could not be determined")

// Neo4jEditionInfo encapsulates information about the detected Neo4j edition.
//
// The three fields answer different questions, and the difference is the whole
// point of the type:
//
//   - IsDetected says whether the edition was established at all. detectNeo4jEdition
//     runs `cypher-shell` through an exec, so a pod restarting, a connection
//     refused or an auth blip all end here.
//   - IsCommunity is the *capture method* decision, and it is the destructive
//     one: Community has no online backup, so it is answered by stopping
//     Infrahub and taking Neo4j offline for a cold dump. It is therefore never
//     true unless the edition was determined — a probe that failed is not
//     evidence of Community, and reading it as Community turned a momentary
//     `cypher-shell` failure against an internal Enterprise database into an
//     unnecessary outage plus an offline dump.
//   - Edition keeps the conservative *restore* fallback on an undetermined
//     probe. Restoring a Community dump with the Community method is the safe
//     reading of an unknown edition and does nothing to the running instance;
//     the asymmetry with IsCommunity is deliberate, not an oversight.
type Neo4jEditionInfo struct {
	Edition     string
	IsCommunity bool
	IsDetected  bool
}

// NewNeo4jEditionInfo creates a new Neo4jEditionInfo from an edition string
func NewNeo4jEditionInfo(edition string, err error) *Neo4jEditionInfo {
	if err != nil {
		logrus.Infof("could not detect neo4j edition: %v", err)
		return &Neo4jEditionInfo{
			Edition:     neo4jEditionCommunity, // Restore-side fallback only; see the type's doc comment.
			IsCommunity: false,
			IsDetected:  false,
		}
	}

	normalized := strings.ToLower(edition)
	return &Neo4jEditionInfo{
		Edition:     normalized,
		IsCommunity: isCommunityEdition(normalized),
		IsDetected:  true,
	}
}

// RequiresOfflineCapture reports whether the backup has to stop Infrahub and
// take Neo4j offline for a cold dump, which is true only of a Community
// edition that was actually determined to be one.
func (info *Neo4jEditionInfo) RequiresOfflineCapture() bool {
	return info.IsDetected && info.IsCommunity
}

// requireDeterminedEdition refuses a backup whose edition is unknown, before
// the run has stopped anything.
//
// Neither capture method is safe to assume here: Community's offline dump costs
// an outage the deployment may not need, and Enterprise's online backup would
// silently produce an inconsistent capture of a Community database, which
// constitution Principle II forbids more strongly than it forbids failing. So
// the run stops and says what to check.
func (info *Neo4jEditionInfo) requireDeterminedEdition() error {
	if info.IsDetected {
		return nil
	}

	return fmt.Errorf(
		"cannot start the backup: %w, so this run cannot tell whether Neo4j has to be taken offline for a consistent capture; "+
			"no Infrahub service was stopped. The edition is read by running `cypher-shell` in the database container, so check that "+
			"the database is running and that its credentials are right (INFRAHUB_DB_USERNAME / INFRAHUB_DB_PASSWORD), then retry",
		errNeo4jEditionUndetermined)
}

// LogDetection logs the detection result
func (info *Neo4jEditionInfo) LogDetection(context string) {
	if !info.IsDetected {
		logrus.Warnf("Could not determine Neo4j edition during %s; the capture method cannot be chosen from a probe that did not answer", context)
	} else {
		logrus.Infof("Detected Neo4j %s edition for %s", info.Edition, context)
	}
}

// ResolveRestoreEdition determines the correct edition to use for restore
func (info *Neo4jEditionInfo) ResolveRestoreEdition(backupEdition string) (string, error) {
	backupNormalized := strings.ToLower(backupEdition)

	// If backup is community and detected is enterprise, always use community method
	if backupNormalized == neo4jEditionCommunity && info.Edition == neo4jEditionEnterprise {
		logrus.Info("Backup is Community edition; will use community restore method")
		return neo4jEditionCommunity, nil
	}

	// Cannot restore Enterprise backup on Community edition
	if backupNormalized == neo4jEditionEnterprise && info.Edition == neo4jEditionCommunity {
		return "", fmt.Errorf("cannot restore Enterprise backup on Community edition Neo4j")
	}

	// Use detected edition
	return info.Edition, nil
}

// detectNeo4jEditionInfo detects the Neo4j edition and returns structured information
func (iops *InfrahubOps) detectNeo4jEditionInfo(context string) *Neo4jEditionInfo {
	edition, err := iops.detectNeo4jEdition()
	info := NewNeo4jEditionInfo(edition, err)
	info.LogDetection(context)
	return info
}

func (iops *InfrahubOps) detectNeo4jEdition() (string, error) {
	// A database that lives outside the deployment has no container to run
	// `cypher-shell` in, so the exec below would fail and be read as "edition
	// undetermined" — which stops the run in requireDeterminedEdition, naming a
	// container that was never going to be there. The edition of such a database
	// was read over Bolt by the probe the gate ran, before anything was stopped;
	// this returns that answer rather than asking a second time. Both gates
	// probe — a capture's and a restore's — and this decision is reached from
	// both directions, so it reads the record of either rather than the
	// capture's alone, which is what left a restore against an external Neo4j
	// with no edition and no way to proceed (T148).
	source, err := iops.externalDatabaseFor(serviceNeo4j)
	if err != nil {
		return "", err
	}
	if source != nil {
		if source.Facts.Edition == "" {
			return "", fmt.Errorf("the external %s server did not report its edition", serviceNeo4j)
		}

		return source.Facts.Edition, nil
	}

	output, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"CALL dbms.components() YIELD edition",
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to query neo4j edition: %w", err)
	}

	edition := extractNeo4jEdition(output)
	if edition == "" {
		return "", fmt.Errorf("unable to parse neo4j edition from output: %s", strings.TrimSpace(output))
	}

	return edition, nil
}

func extractNeo4jEdition(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(strings.Trim(lines[i], "\""))
		if trimmed != "" {
			return strings.ToLower(trimmed)
		}
	}
	return ""
}

func (iops *InfrahubOps) generateBackupFilename() string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("infrahub_backup_%s.tar.gz", timestamp)
}

func (iops *InfrahubOps) createBackupMetadata(backupID string, includeTaskManager bool, infrahubVersion string, neo4jEdition string) *BackupMetadata {
	components := []string{"database"}
	if includeTaskManager {
		components = append(components, "task-manager-db")
	}

	metadata := &BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		InfrahubVersion: infrahubVersion,
		Components:      components,
		Neo4jEdition:    strings.ToLower(neo4jEdition),
	}

	iops.applySourceProvenance(metadata, components)

	return metadata
}

// applyCaptureCompleteness records what this run's captures reported (FR-012).
//
// It is called where the metadata is *serialized*, not where it is built, and
// that is the whole of it. createBackupMetadata runs before backupDatabase and
// backupTaskManagerDB, so incompleteCaptures is necessarily empty when it runs
// and the field it wrote was a prediction rather than a report — `true` in
// every artefact ever produced, whatever the captures went on to say. Read from
// here the captures have finished, so a verdict any of them recorded is already
// in.
//
// Both backends still gate on the same verdict before reaching serialization:
// the tarball path discards rather than archiving, and the Plakar path refuses
// before the metadata component that would carry the field is created. So this
// tool does not write `false` — which is FR-012's actual requirement, that an
// incomplete capture leaves nothing behind rather than something labelled. What
// this makes true is that the label cannot disagree with the gate, whatever a
// later change does to where the gate sits; and refuseIncompleteCapture, its
// reader, is left guarding the artefacts a *failed removal* or an older tool can
// leave behind, against a value that now means what it says.
//
// A run with no external database writes nothing, exactly as
// applySourceProvenance writes nothing, and for the same reason (FR-015).
func (iops *InfrahubOps) applyCaptureCompleteness(metadata *BackupMetadata) {
	if len(iops.externalSources) == 0 {
		return
	}

	complete := iops.capturesComplete()
	metadata.CaptureComplete = &complete
}

// applySourceProvenance records where this capture's data came from, for a run
// that read at least one database living outside the deployment (FR-005).
//
// A run whose databases are all internal returns here having written nothing, so
// its metadata is byte-for-byte what it has always been (FR-015). That is also
// the honest encoding rather than a convenience: the provenance fields exist to
// describe an endpoint the artefact does not otherwise account for, and an
// internal deployment has none — which is precisely the reading
// contracts/artifact-and-metadata.md gives to their absence.
//
// Within a run that did read an external database, every database is recorded,
// internal ones included, so the fields themselves never signal externality.
//
// services are the deployment service names this run captured, which is what
// endpoints resolve under. They are taken as an argument rather than read from
// metadata.Components because the Plakar backend replaces that list with its own
// component names after this has run, and a key nothing resolves under describes
// nothing.
func (iops *InfrahubOps) applySourceProvenance(metadata *BackupMetadata, services []string) {
	if len(iops.externalSources) == 0 {
		return
	}

	metadata.ExternalLocation = make(map[string]bool, len(services))

	for _, service := range services {
		source := iops.externalSourceFor(service)
		metadata.ExternalLocation[service] = source != nil
		if source == nil {
			continue
		}

		if endpoints := renderHostPortList(source.captureTargets()); len(endpoints) > 0 {
			if metadata.SourceEndpoints == nil {
				metadata.SourceEndpoints = map[string][]string{}
			}
			metadata.SourceEndpoints[service] = endpoints
		}

		// Absent rather than a map of unknowns for a database whose roles this
		// run never asked for; see probedFacts.RolesObserved.
		if !source.Facts.RolesObserved {
			continue
		}

		if roles := source.observedRoles(); len(roles) > 0 {
			if metadata.SourceRoles == nil {
				metadata.SourceRoles = map[string]map[string]string{}
			}
			metadata.SourceRoles[service] = roles
		}
	}
}
