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
}

// Neo4jEditionInfo encapsulates information about the detected Neo4j edition
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
			Edition:     neo4jEditionCommunity, // Default fallback
			IsCommunity: true,
			IsDetected:  false,
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
		logrus.Warnf("Could not determine Neo4j edition during %s; defaulting to community workflow", context)
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
