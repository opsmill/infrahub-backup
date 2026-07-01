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
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Connector string    `json:"connector"`
	// Edition is "enterprise" for the online protocol (online backup is
	// Enterprise-only) and omitted for offline, where the dump works on either
	// edition and we cannot tell without probing.
	Edition       string `json:"edition,omitempty"`
	Database      string `json:"database"`
	Host          string `json:"host,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	BackupMode    string `json:"backup_mode"` // "online" | "offline"
}

// Emit builds the manifest and sends /manifest.json as the first record.
// Failures collecting optional metadata are non-fatal (partial manifests are OK).
func Emit(ctx context.Context, conn neo4jconn.ConnConfig, records chan<- *connectors.Record) error {
	mode, edition := "online", "enterprise" // online backup is Enterprise-only
	if conn.Offline() {
		mode, edition = "offline", "" // offline dump runs on either edition; unknown here
	}
	m := &Manifest{
		Version:       1,
		CreatedAt:     time.Now().UTC(),
		Connector:     "neo4j",
		Edition:       edition,
		Database:      conn.Database,
		Host:          conn.Host,
		ServerVersion: conn.ServerVersion(ctx),
		BackupMode:    mode,
	}

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
