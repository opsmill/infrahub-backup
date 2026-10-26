package app

import (
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
}

// Neo4jEditionInfo encapsulates information about the detected Neo4j edition
type Neo4jEditionInfo struct {
	Edition     string
	IsCommunity bool
	IsDetected  bool
	// DetectionErr is why the edition could not be detected; nil when IsDetected.
	DetectionErr error
}

// NewNeo4jEditionInfo creates a new Neo4jEditionInfo from an edition string. When detection
// failed, Edition and IsCommunity fall back to Community, but IsDetected is false and
// ResolveRestoreEdition treats the edition as unknown.
func NewNeo4jEditionInfo(edition string, err error) *Neo4jEditionInfo {
	if err != nil {
		return &Neo4jEditionInfo{
			Edition:      neo4jEditionCommunity, // Default fallback
			IsCommunity:  true,
			IsDetected:   false,
			DetectionErr: err,
		}
	}

	normalized := strings.ToLower(edition)
	return &Neo4jEditionInfo{
		Edition:     normalized,
		IsCommunity: strings.EqualFold(normalized, neo4jEditionCommunity),
		IsDetected:  true,
	}
}

// LogDetection logs the detection result
func (info *Neo4jEditionInfo) LogDetection(context string) {
	if !info.IsDetected {
		logrus.Warnf("Could not determine Neo4j edition during %s; defaulting to community workflow: %v", context, info.DetectionErr)
	} else {
		logrus.Infof("Detected Neo4j %s edition for %s", info.Edition, context)
	}
}

// ResolveRestoreEdition determines the correct edition to use for restore
func (info *Neo4jEditionInfo) ResolveRestoreEdition(backupEdition string) (string, error) {
	backupNormalized := strings.ToLower(backupEdition)

	// An edition the probe could not detect is unknown, not Community. A Community backup is
	// restored with the community method on either edition, and that method does not log in
	// to Neo4j, so it can still proceed. Any other backup needs the edition to choose a method.
	if !info.IsDetected {
		if backupNormalized == neo4jEditionCommunity {
			return neo4jEditionCommunity, nil
		}
		return "", fmt.Errorf("could not detect the Neo4j edition of the target deployment, which the restore method for this backup depends on: %w", info.DetectionErr)
	}

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

// detectBackupNeo4jEdition detects the Neo4j edition a backup is taken with.
// A backup never falls back to Community when detection fails: the Community
// workflow stops the Infrahub services, so a failed query (for example one sent
// to a pod that is not Neo4j) must not cause an outage.
func (iops *InfrahubOps) detectBackupNeo4jEdition() (*Neo4jEditionInfo, error) {
	edition, err := iops.detectNeo4jEdition()
	if err != nil {
		return nil, fmt.Errorf("could not determine the Neo4j edition, so the backup was not started and no service was stopped; check that the database service resolves to the Neo4j server and that the Neo4j credentials are correct: %w", err)
	}
	info := NewNeo4jEditionInfo(edition, nil)
	info.LogDetection("backup")
	return info, nil
}

func (iops *InfrahubOps) detectNeo4jEdition() (string, error) {
	output, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"CALL dbms.components() YIELD edition",
	}, nil)
	if err != nil {
		if line := commandErrorLine(output); line != "" {
			err = fmt.Errorf("failed to query neo4j edition: %w: %s", err, line)
		} else {
			err = fmt.Errorf("failed to query neo4j edition: %w", err)
		}
		if iops.neo4jCredentialLookupErr != nil {
			// The query logged in with a default username or password, which is the likely
			// reason Neo4j refused it and the one the operator can act on.
			err = fmt.Errorf("%w\nThe query used the default Neo4j credentials because the lookup failed: %v\n"+
				"To use the deployment's credentials, start infrahub-server or set INFRAHUB_DB_USERNAME and INFRAHUB_DB_PASSWORD",
				err, iops.neo4jCredentialLookupErr)
		}
		return "", err
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

	return &BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		InfrahubVersion: infrahubVersion,
		Components:      components,
		Neo4jEdition:    strings.ToLower(neo4jEdition),
	}
}
