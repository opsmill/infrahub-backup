package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// Plakar component names.
const (
	ComponentNeo4j    = "neo4j"
	ComponentPostgres = "postgres"
	ComponentMetadata = "metadata"
)

// Backup group status values.
const (
	StatusComplete   = "complete"
	StatusIncomplete = "incomplete"
)

// Plakar snapshot tag keys.
const (
	TagBackupID         = "infrahub.backup-id"
	TagComponent        = "infrahub.component"
	TagBackupStatus     = "infrahub.backup-status"
	TagVersion          = "infrahub.version"
	TagToolVersion      = "infrahub.backup-tool-version"
	TagNeo4jEdition     = "infrahub.neo4j-edition"
	TagComponents       = "infrahub.components"
	TagRedacted         = "infrahub.redacted"
)

// SnapshotInfo holds displayable information about a single Plakar snapshot.
type SnapshotInfo struct {
	SnapshotID string `json:"snapshot_id"`
	Component  string `json:"component"`
	MAC        objects.MAC `json:"-"`
}

// BackupGroupInfo holds displayable information about a backup group.
type BackupGroupInfo struct {
	BackupID        string         `json:"backup_id"`
	Date            string         `json:"date"`
	Status          string         `json:"status"`
	InfrahubVersion string         `json:"infrahub_version"`
	Neo4jEdition    string         `json:"neo4j_edition"`
	Components      []string       `json:"components"`
	Snapshots       []SnapshotInfo `json:"snapshots"`
	Redacted        bool           `json:"redacted,omitempty"`
	Timestamp       time.Time      `json:"-"`
}

// collectBackupGroups reads all snapshots from a repository and groups them by backup-id tag.
func collectBackupGroups(repo *repository.Repository) ([]BackupGroupInfo, error) {
	groupMap := make(map[string]*BackupGroupInfo)

	for mac, listErr := range repo.ListSnapshots() {
		if listErr != nil {
			logrus.Warnf("Failed to list snapshot: %v", listErr)
			continue
		}
		snap, err := snapshot.Load(repo, mac)
		if err != nil {
			logrus.Warnf("Failed to load snapshot %x: %v", mac[:8], err)
			continue
		}

		tags := parseSnapshotTags(snap.Header.Tags)
		backupID := tags[TagBackupID]
		component := tags[TagComponent]

		// Skip snapshots without backup-id (old format)
		if backupID == "" {
			snap.Close()
			continue
		}

		group, exists := groupMap[backupID]
		if !exists {
			group = &BackupGroupInfo{
				BackupID:        backupID,
				Date:            snap.Header.Timestamp.Format(time.RFC3339),
				Timestamp:       snap.Header.Timestamp,
				InfrahubVersion: tags[TagVersion],
				Neo4jEdition:    tags[TagNeo4jEdition],
				Redacted:        tags[TagRedacted] == "true",
			}
			if comps := tags[TagComponents]; comps != "" {
				group.Components = strings.Split(comps, ",")
			}
			groupMap[backupID] = group
		}

		if component != "" {
			group.Snapshots = append(group.Snapshots, SnapshotInfo{
				SnapshotID: fmt.Sprintf("%x", mac[:8]),
				Component:  component,
				MAC:        mac,
			})
		}

		snap.Close()
	}

	// Determine status and build sorted list
	var groups []BackupGroupInfo
	for _, group := range groupMap {
		group.Status = determineGroupStatus(group)
		groups = append(groups, *group)
	}

	// Sort by timestamp descending (newest first)
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Timestamp.After(groups[j].Timestamp)
	})

	return groups, nil
}

// determineGroupStatus checks if all expected components are present.
func determineGroupStatus(group *BackupGroupInfo) string {
	if len(group.Components) == 0 {
		if len(group.Snapshots) > 0 {
			return StatusComplete
		}
		return StatusIncomplete
	}

	presentComponents := make(map[string]bool)
	for _, snap := range group.Snapshots {
		presentComponents[snap.Component] = true
	}

	for _, expected := range group.Components {
		if !presentComponents[expected] {
			return StatusIncomplete
		}
	}
	return StatusComplete
}

// findBackupGroup finds a specific backup group by ID.
func findBackupGroup(repo *repository.Repository, backupID string) (*BackupGroupInfo, error) {
	groups, err := collectBackupGroups(repo)
	if err != nil {
		return nil, err
	}

	for i := range groups {
		if groups[i].BackupID == backupID {
			return &groups[i], nil
		}
	}

	// Build helpful error with available groups
	var available []string
	for _, g := range groups {
		available = append(available, fmt.Sprintf("%s (%s, %s)", g.BackupID, g.Date, g.Status))
	}
	if len(available) > 0 {
		return nil, fmt.Errorf("backup group not found: %s\nAvailable groups:\n  %s", backupID, strings.Join(available, "\n  "))
	}
	return nil, fmt.Errorf("backup group not found: %s (repository contains no backup groups)", backupID)
}

// findLatestCompleteGroup returns the most recent complete backup group.
// If no complete groups exist, returns the most recent incomplete group (check group.Status).
func findLatestCompleteGroup(repo *repository.Repository) (*BackupGroupInfo, error) {
	groups, err := collectBackupGroups(repo)
	if err != nil {
		return nil, err
	}

	if len(groups) == 0 {
		return nil, fmt.Errorf("no backup groups found in repository")
	}

	// Groups are already sorted newest-first
	for i := range groups {
		if groups[i].Status == StatusComplete {
			return &groups[i], nil
		}
	}

	// No complete groups — return newest incomplete
	return &groups[0], nil
}

// missingComponents returns the list of expected components not present in the group.
func missingComponents(group *BackupGroupInfo) []string {
	present := make(map[string]bool)
	for _, snap := range group.Snapshots {
		present[snap.Component] = true
	}

	var missing []string
	for _, expected := range group.Components {
		if !present[expected] {
			missing = append(missing, expected)
		}
	}
	return missing
}

// parseSnapshotTags converts a tag slice into a map.
func parseSnapshotTags(tags []string) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		parts := strings.SplitN(tag, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// ListSnapshots lists all backup groups in a Plakar repository.
func (iops *InfrahubOps) ListSnapshots(jsonOutput bool) error {
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return fmt.Errorf("failed to initialize plakar context: %w", err)
	}
	defer closePlakarContext(kctx)

	repo, err := openRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	groups, err := collectBackupGroups(repo)
	if err != nil {
		return err
	}

	if len(groups) == 0 {
		logrus.Info("No backups found in repository")
		return nil
	}

	if jsonOutput {
		data, err := json.MarshalIndent(groups, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal backup group list: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	// Text table output — grouped by backup-id
	fmt.Printf("%-20s  %-25s  %-10s  %-17s  %-14s  %s\n",
		"BACKUP ID", "DATE", "STATUS", "INFRAHUB VERSION", "NEO4J EDITION", "COMPONENTS")
	for _, g := range groups {
		components := make([]string, 0, len(g.Snapshots))
		for _, s := range g.Snapshots {
			components = append(components, s.Component)
		}
		fmt.Printf("%-20s  %-25s  %-10s  %-17s  %-14s  %s\n",
			g.BackupID, g.Date, g.Status, g.InfrahubVersion, g.Neo4jEdition, strings.Join(components, ", "))
	}

	return nil
}
