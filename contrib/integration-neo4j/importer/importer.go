// Package importer implements the Neo4j Plakar importer for two protocols:
//   - neo4j://[user:pass@]host:6362/<db>      Enterprise ONLINE backup
//   - neo4j+offline:///<datadir>?database=<db> Community OFFLINE dump (DB must be stopped)
//
// Both run neo4j-admin into a temporary directory and stream the produced files
// into the snapshot. The integration NEVER manages DB lifecycle — for the
// offline protocol the caller must stop the database first.
package importer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-neo4j/manifest"
	"github.com/PlakarKorp/integration-neo4j/neo4jconn"
	"github.com/PlakarKorp/kloset/connectors"
	iimporter "github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/location"
	"github.com/PlakarKorp/kloset/objects"
)

func init() {
	iimporter.Register("neo4j", location.FLAG_STREAM, NewImporter)
	iimporter.Register("neo4j+offline", location.FLAG_STREAM, NewImporter)
}

// Importer backs up a single Neo4j database.
type Importer struct {
	conn            neo4jconn.ConnConfig
	includeMetadata string // online only: none|all|users|roles (empty = omit the flag)
}

// NewImporter is the connector constructor used both for in-process registration
// (init above) and by the plugin entrypoint (sdk.EntrypointImporter).
func NewImporter(_ context.Context, _ *connectors.Options, proto string, config map[string]string) (iimporter.Importer, error) {
	conn, err := neo4jconn.ParseConnConfig(proto, config)
	if err != nil {
		return nil, err
	}
	imp := &Importer{conn: conn}
	if v := config["include_metadata"]; v != "" {
		switch v {
		case "none", "all", "users", "roles":
			imp.includeMetadata = v
		default:
			return nil, fmt.Errorf("invalid include_metadata %q: want none|all|users|roles", v)
		}
	}
	return imp, nil
}

func (i *Importer) Origin() string {
	return i.conn.Proto + "://" + i.conn.Host + "/" + i.conn.Database
}
func (i *Importer) Type() string                   { return i.conn.Proto }
func (i *Importer) Root() string                   { return "/" }
func (i *Importer) Flags() location.Flags          { return location.FLAG_STREAM }
func (i *Importer) Ping(ctx context.Context) error { return i.conn.Ping(ctx) }
func (i *Importer) Close(_ context.Context) error  { return nil }

var _ iimporter.Importer = (*Importer)(nil)

// Import emits /manifest.json then runs neo4j-admin into a temp dir and streams
// the produced files as records (one per file).
func (i *Importer) Import(ctx context.Context, records chan<- *connectors.Record, _ <-chan *connectors.Result) error {
	defer close(records)

	if err := manifest.Emit(ctx, i.conn, records); err != nil {
		return fmt.Errorf("emitting manifest: %w", err)
	}

	tmp, err := os.MkdirTemp("", "plakar-neo4j-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}

	args, err := i.adminArgs(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}

	// NOTE (behavior verification pending): neo4j-admin is run eagerly to the temp
	// dir, then its output files are streamed lazily. The exact produced layout
	// (a .dump file for offline; a backup artifact directory for online) and the
	// precise flags must be validated against a real Neo4j (testcontainers).
	cmd := exec.CommandContext(ctx, i.conn.BinPath(), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("neo4j-admin %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return emitDir(ctx, records, tmp)
}

// adminArgs builds the neo4j-admin command for the active protocol.
func (i *Importer) adminArgs(toPath string) ([]string, error) {
	db := i.conn.Database
	switch i.conn.Proto {
	case "neo4j+offline":
		// Community OFFLINE dump. Requires the database STOPPED (caller's responsibility).
		return []string{"database", "dump", "--to-path=" + toPath, db}, nil
	case "neo4j":
		// Enterprise ONLINE backup. --compress=false keeps the artifact dedup-friendly.
		// VERIFIED against Neo4j Enterprise 2025.10.1: these flags produce a single
		// "<db>-<timestamp>.backup" artifact under --to-path (emitDir walks it).
		args := []string{"database", "backup", "--to-path=" + toPath, "--compress=false"}
		if i.conn.Host != "" {
			from := i.conn.Host
			if i.conn.Port != "" {
				from = i.conn.Host + ":" + i.conn.Port
			}
			args = append(args, "--from="+from)
		}
		if i.includeMetadata != "" {
			args = append(args, "--include-metadata="+i.includeMetadata)
		}
		return append(args, db), nil
	default:
		return nil, fmt.Errorf("unsupported protocol %q", i.conn.Proto)
	}
}

// emitDir walks dir and emits one record per file. The temp dir is removed once
// every emitted file's reader has been closed (refcount).
func emitDir(ctx context.Context, records chan<- *connectors.Record, dir string) error {
	var files []string
	if err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("walking neo4j-admin output: %w", err)
	}
	if len(files) == 0 {
		os.RemoveAll(dir)
		return fmt.Errorf("neo4j-admin produced no output in %s", dir)
	}

	var mu sync.Mutex
	remaining := len(files)
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		remaining--
		if remaining == 0 {
			os.RemoveAll(dir)
		}
	}

	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			os.RemoveAll(dir)
			return err
		}
		pathname := "/" + filepath.ToSlash(rel)
		fi, err := os.Stat(f)
		if err != nil {
			os.RemoveAll(dir)
			return err
		}
		fileinfo := objects.FileInfo{
			Lname:    path.Base(pathname),
			Lsize:    fi.Size(),
			Lmode:    0444,
			LmodTime: time.Now().UTC(),
		}
		fpath := f
		readerFunc := func() (io.ReadCloser, error) {
			fh, err := os.Open(fpath)
			if err != nil {
				return nil, err
			}
			return &cleanupReader{ReadCloser: fh, cleanup: cleanup}, nil
		}
		select {
		case <-ctx.Done():
			os.RemoveAll(dir)
			return ctx.Err()
		case records <- connectors.NewRecord(pathname, "", fileinfo, nil, readerFunc):
		}
	}
	return nil
}

// cleanupReader runs cleanup exactly once when the file reader is closed.
type cleanupReader struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
}

func (r *cleanupReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cleanup)
	return err
}
