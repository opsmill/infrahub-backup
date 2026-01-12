package app

import (
	"fmt"
	"strings"
	"time"
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
