package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// SnapshotInfo holds displayable information about a Plakar snapshot.
type SnapshotInfo struct {
	SnapshotID      string   `json:"snapshot_id"`
	Date            string   `json:"date"`
	InfrahubVersion string   `json:"infrahub_version"`
	Neo4jEdition    string   `json:"neo4j_edition"`
	Components      []string `json:"components"`
}

// ListSnapshots lists all snapshots in a Plakar repository with Infrahub metadata.
func (iops *InfrahubOps) ListSnapshots(jsonOutput bool) error {
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

	var snapshots []SnapshotInfo

	for mac := range repo.ListSnapshots() {
		snap, err := snapshot.Load(repo, mac)
		if err != nil {
			logrus.Warnf("Failed to load snapshot %x: %v", mac[:8], err)
			continue
		}

		info := SnapshotInfo{
			SnapshotID: fmt.Sprintf("%x", mac[:8]),
			Date:       snap.Header.Timestamp.Format(time.RFC3339),
		}

		// Extract Infrahub metadata from tags
		for _, tag := range snap.Header.Tags {
			parts := strings.SplitN(tag, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "infrahub.version":
				info.InfrahubVersion = parts[1]
			case "infrahub.neo4j-edition":
				info.Neo4jEdition = parts[1]
			case "infrahub.components":
				info.Components = strings.Split(parts[1], ",")
			}
		}

		snapshots = append(snapshots, info)
		snap.Close()
	}

	if len(snapshots) == 0 {
		logrus.Info("No snapshots found in repository")
		return nil
	}

	if jsonOutput {
		data, err := json.MarshalIndent(snapshots, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal snapshot list: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	// Text table output
	fmt.Printf("%-16s  %-25s  %-17s  %-14s  %s\n",
		"SNAPSHOT ID", "DATE", "INFRAHUB VERSION", "NEO4J EDITION", "COMPONENTS")
	for _, s := range snapshots {
		components := strings.Join(s.Components, ", ")
		fmt.Printf("%-16s  %-25s  %-17s  %-14s  %s\n",
			s.SnapshotID, s.Date, s.InfrahubVersion, s.Neo4jEdition, components)
	}

	return nil
}
