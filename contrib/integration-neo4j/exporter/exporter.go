// Package exporter implements the Neo4j Plakar exporter (restore):
//   - neo4j://         -> neo4j-admin database restore --from-path (Enterprise)
//   - neo4j+offline:// -> neo4j-admin database load   --from-path (Community)
//
// Snapshot files are staged to a temp directory as they arrive, then a single
// neo4j-admin restore/load runs once the record stream closes. The integration
// does NOT manage DB lifecycle — the target database must be stopped.
package exporter

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/PlakarKorp/integration-neo4j/neo4jconn"
	"github.com/PlakarKorp/kloset/connectors"
	eexporter "github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/location"
)

func init() {
	eexporter.Register("neo4j", 0, NewExporter)
	eexporter.Register("neo4j+offline", 0, NewExporter)
}

// Exporter restores a single Neo4j database from a snapshot.
type Exporter struct {
	conn      neo4jconn.ConnConfig
	overwrite bool
	stageDir  string
}

// NewExporter is the connector constructor (in-process registration + plugin entrypoint).
func NewExporter(_ context.Context, _ *connectors.Options, proto string, config map[string]string) (eexporter.Exporter, error) {
	conn, err := neo4jconn.ParseConnConfig(proto, config)
	if err != nil {
		return nil, err
	}
	e := &Exporter{conn: conn}
	if v := config["overwrite"]; strings.EqualFold(v, "true") {
		e.overwrite = true
	}
	return e, nil
}

func (e *Exporter) Origin() string                 { return e.conn.Origin() }
func (e *Exporter) Type() string                   { return e.conn.Proto }
func (e *Exporter) Root() string                   { return "/" }
func (e *Exporter) Flags() location.Flags          { return 0 }
func (e *Exporter) Ping(ctx context.Context) error { return e.conn.Ping(ctx) }
func (e *Exporter) Close(_ context.Context) error {
	if e.stageDir != "" {
		return os.RemoveAll(e.stageDir)
	}
	return nil
}

var _ eexporter.Exporter = (*Exporter)(nil)

// Export stages incoming records to a temp dir, then runs neo4j-admin restore/load.
func (e *Exporter) Export(ctx context.Context, records <-chan *connectors.Record, results chan<- *connectors.Result) error {
	defer close(results)

	stage, err := os.MkdirTemp("", "plakar-neo4j-restore-*")
	if err != nil {
		return fmt.Errorf("creating stage dir: %w", err)
	}
	e.stageDir = stage

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case record, ok := <-records:
			if !ok {
				// Stream closed: all files staged — run the restore/load.
				return e.load(ctx, stage)
			}
			results <- e.stageRecord(record, stage)
		}
	}
}

// stageRecord writes one record to the stage dir (skipping dirs and the manifest).
func (e *Exporter) stageRecord(record *connectors.Record, stage string) *connectors.Result {
	if record.FileInfo.Lmode.IsDir() {
		return record.Ok()
	}
	if filepath.Base(record.Pathname) == "manifest.json" {
		return record.Ok() // metadata only
	}
	if record.Reader == nil {
		return record.Error(fmt.Errorf("record %s has no reader", record.Pathname))
	}
	dst := filepath.Join(stage, filepath.FromSlash(strings.TrimPrefix(record.Pathname, "/")))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return record.Error(err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return record.Error(err)
	}
	if _, err := io.Copy(f, record.Reader); err != nil {
		f.Close()
		return record.Error(err)
	}
	if err := f.Close(); err != nil {
		return record.Error(err)
	}
	return record.Ok()
}

// load runs neo4j-admin restore (online layout) or load (offline dump).
//
// NOTE (behavior verification pending): the exact subcommand selection and flags
// must be validated against a real Neo4j. We dispatch on whether a single *.dump
// file was staged (offline load) versus a backup artifact (online restore).
func (e *Exporter) load(ctx context.Context, stage string) error {
	db := e.conn.Database
	// Locate the staged backup artifact (the single non-manifest data file).
	// VERIFIED finding (2026-06-30): pass the artifact FILE — not the stage
	// directory — to --from-path. Given a directory, neo4j-admin matches artifacts
	// by the *target* database name and fails ("no backups to restore for <db>")
	// when restoring to a differently-named target; given the file it restores to
	// any target name.
	// Exactly one data artifact is expected (manifest.json is skipped at staging):
	// the offline "<db>.dump" or the online ".backup" artifact. Fail clearly on 0
	// or >1 rather than silently picking the alphabetically-first entry.
	entries, err := os.ReadDir(stage)
	if err != nil {
		return fmt.Errorf("reading stage dir: %w", err)
	}
	var artifacts []string
	for _, en := range entries {
		if !en.IsDir() {
			artifacts = append(artifacts, filepath.Join(stage, en.Name()))
		}
	}
	if len(artifacts) != 1 {
		return fmt.Errorf("expected exactly one staged backup artifact in %s, found %d", stage, len(artifacts))
	}
	artifact := artifacts[0]

	// VERIFIED finding (2026-06-30): `database restore` (Enterprise) accepts the
	// artifact FILE in --from-path (a directory would match artifacts by the target
	// db name and fail on a renamed target); `database load` (Community) requires
	// --from-path to be the DIRECTORY containing "<targetdb>.dump". The `--`
	// separator guards a "-"-prefixed database name from being parsed as a flag.
	var args []string
	if e.conn.Offline() {
		// `database load` looks for "<targetdb>.dump" in --from-path, but the staged
		// dump keeps the SOURCE database name — rename it when restoring into a
		// differently-named database.
		want := filepath.Join(stage, db+".dump")
		if artifact != want {
			if err := os.Rename(artifact, want); err != nil {
				return fmt.Errorf("staging dump as %q: %w", db+".dump", err)
			}
		}
		args = []string{"database", "load", "--from-path=" + stage}
	} else {
		args = []string{"database", "restore", "--from-path=" + artifact}
	}
	if e.overwrite {
		args = append(args, "--overwrite-destination=true")
	}
	args = append(args, "--", db)

	cmd := exec.CommandContext(ctx, e.conn.BinPath(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("neo4j-admin %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
