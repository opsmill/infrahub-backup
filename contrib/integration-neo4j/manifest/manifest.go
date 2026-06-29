// Package manifest builds and emits the /manifest.json record written before the
// backup data in every Neo4j snapshot.
package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/PlakarKorp/integration-neo4j/neo4jconn"
	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/objects"
)

// Manifest is the structure serialised to /manifest.json.
type Manifest struct {
	Version       int       `json:"version"`
	CreatedAt     time.Time `json:"created_at"`
	Connector     string    `json:"connector"`
	Edition       string    `json:"edition,omitempty"` // "enterprise" | "community" | "" (unknown)
	Database      string    `json:"database"`
	Host          string    `json:"host,omitempty"`
	ServerVersion string    `json:"server_version,omitempty"`
	BackupMode    string    `json:"backup_mode"` // "online" | "offline"
	// NodeCount/RelationshipCount are populated only when a live Bolt connection
	// is reachable (Enterprise online). Omitted for Community offline (DB stopped).
	NodeCount         *int64 `json:"node_count,omitempty"`
	RelationshipCount *int64 `json:"relationship_count,omitempty"`
}

// Emit builds the manifest and sends /manifest.json as the first record.
// Failures collecting optional metadata are non-fatal (partial manifests are OK).
func Emit(ctx context.Context, conn neo4jconn.ConnConfig, records chan<- *connectors.Record) error {
	mode := "online"
	if conn.Offline() {
		mode = "offline"
	}
	m := &Manifest{
		Version:       1,
		CreatedAt:     time.Now().UTC(),
		Connector:     "neo4j",
		Database:      conn.Database,
		Host:          conn.Host,
		ServerVersion: conn.ServerVersion(ctx),
		BackupMode:    mode,
	}
	// TODO (behavior verification pending): derive Edition from the version string
	// or config, and populate NodeCount/RelationshipCount via a Bolt query when
	// online. Requires a Bolt driver dependency + a live-DB test harness.

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest: marshalling JSON: %w", err)
	}
	fileinfo := objects.FileInfo{
		Lname:    "manifest.json",
		Lsize:    int64(len(data)),
		Lmode:    0444,
		LmodTime: time.Now().UTC(),
	}
	readerFunc := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case records <- connectors.NewRecord("/manifest.json", "", fileinfo, nil, readerFunc):
	}
	return nil
}
