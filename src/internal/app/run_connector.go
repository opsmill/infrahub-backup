package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// RunConnectorCommand returns the hidden `__run-connector` worker command.
//
// It is the dependency-light worker that runs INSIDE the co-located runner
// (a one-shot container next to the database): it opens the kloset repository
// and performs ONE connector backup or restore operation, dispatching to the
// registered connector by the URI scheme. It has NO docker/kubectl dependency
// (those live in the orchestration path), so the runner image stays small.
//
//	infrahub-backup __run-connector backup  <repo> <source-uri> [--opt k=v ...] [--tag k=v ...]
//	infrahub-backup __run-connector restore <repo> <dest-uri> <snapshot-hex> [--opt k=v ...]
func RunConnectorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "__run-connector",
		Short:        "Internal: run one connector backup/restore op inside the co-located runner",
		Hidden:       true,
		SilenceUsage: true,
	}

	var backupOpts, tags []string
	var backupCreds credentialsInput
	backupCmd := &cobra.Command{
		Use:          "backup <repo> <source-uri>",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			creds, err := backupCreds.read()
			if err != nil {
				return err
			}
			return runConnectorBackup(args[0], args[1], creds, backupCreds.s3Insecure, parseKV(backupOpts), tags)
		},
	}
	backupCmd.Flags().StringArrayVar(&backupOpts, "opt", nil, "connector option key=value (repeatable)")
	backupCmd.Flags().StringArrayVar(&tags, "tag", nil, "snapshot tag key=value (repeatable)")
	backupCreds.addFlags(backupCmd)

	var restoreOpts []string
	var restoreCreds credentialsInput
	var restorePreserveOwner string
	var migrate Neo4jMigration
	restoreCmd := &cobra.Command{
		Use:          "restore <repo> <dest-uri> <snapshot-hex>",
		Args:         cobra.ExactArgs(3),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			creds, err := restoreCreds.read()
			if err != nil {
				return err
			}
			return runConnectorRestore(args[0], args[1], args[2], creds, restoreCreds.s3Insecure,
				restorePreserveOwner, parseKV(restoreOpts), migrate)
		},
	}
	restoreCmd.Flags().StringArrayVar(&restoreOpts, "opt", nil, "connector option key=value (repeatable)")
	restoreCmd.Flags().StringVar(&restorePreserveOwner, "preserve-owner", "",
		"directory whose ownership must survive the restore (the DB data dir, written as root)")
	restoreCmd.Flags().StringVar(&migrate.Format, "migrate-format", "",
		"run `neo4j-admin database migrate --to-format=<format>` after the restore, in the same offline window")
	restoreCmd.Flags().StringVar(&migrate.Database, "migrate-database", "",
		"database to migrate (required with --migrate-format)")
	restoreCreds.addFlags(restoreCmd)

	// launch: exercise the co-located runner launcher through the tool (testing the
	// orchestration path; the create flow will call LaunchComposeBackup directly).
	var launchOpts, launchTags []string
	var launchVolumes bool
	var launchCreds credentialsInput
	launchCmd := &cobra.Command{
		Use:          "launch <project> <db-service> <repo> <source-uri>",
		Args:         cobra.ExactArgs(4),
		Hidden:       true,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			creds, err := launchCreds.read()
			if err != nil {
				return err
			}
			snap, err := LaunchComposeBackup(args[0], args[1], args[2], args[3], creds, parseKV(launchOpts), launchTags, launchVolumes)
			if err != nil {
				return err
			}
			fmt.Println(snap)
			return nil
		},
	}
	launchCmd.Flags().StringArrayVar(&launchOpts, "opt", nil, "connector option key=value (repeatable)")
	launchCmd.Flags().StringArrayVar(&launchTags, "tag", nil, "snapshot tag key=value (repeatable)")
	launchCmd.Flags().BoolVar(&launchVolumes, "volumes-from-db", false, "share the DB container's volumes (neo4j community/restore)")
	launchCreds.addFlags(launchCmd)

	cmd.AddCommand(backupCmd, restoreCmd, launchCmd)
	return cmd
}

// credentialsStdinFlag is the flag that tells the worker its credentials are
// waiting on stdin. Shared so the two ends cannot disagree about the name.
const credentialsStdinFlag = "--credentials-stdin"

// credentialsInput is the worker's side of the credentials channel: the flags that
// say how to obtain them, and the reader that does.
type credentialsInput struct {
	stdin bool
	// s3Insecure is not a secret, so it travels on the command line. It records that
	// the repository URI carried embedded credentials, which storeConfig has always
	// read as "a local S3-compatible store, reached over plain HTTP" — lifting the
	// credentials out of the URI would otherwise silently flip it to TLS.
	s3Insecure bool
}

func (c *credentialsInput) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&c.stdin, strings.TrimPrefix(credentialsStdinFlag, "--"), false,
		"read the repository passphrase and database/object-store credentials from stdin (one JSON object)")
	cmd.Flags().BoolVar(&c.s3Insecure, "s3-insecure", false,
		"reach the s3:// repository over plain HTTP (implied by credentials embedded in the repo URI)")
}

// read decodes the credentials the orchestrator piped in via `docker run -i`, so
// that none of them appears on the worker's command line or in its environment
// (FR-007), either of which `docker inspect` would publish.
//
// An empty stdin is not an error: a plaintext local repository with an
// unauthenticated connector legitimately has nothing to send.
func (c *credentialsInput) read() (runnerCredentials, error) {
	if !c.stdin {
		return runnerCredentials{}, nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return runnerCredentials{}, fmt.Errorf("reading credentials from stdin: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return runnerCredentials{}, nil
	}
	var creds runnerCredentials
	if err := json.Unmarshal([]byte(trimmed), &creds); err != nil {
		// The error deliberately does not echo the payload: it holds the secrets.
		return runnerCredentials{}, fmt.Errorf("decoding credentials from stdin: %w", err)
	}
	return creds, nil
}

func parseKV(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func connectorOptions(kctx *kcontext.KContext) *connectors.Options {
	return &connectors.Options{
		Hostname:       kctx.Hostname,
		CWD:            kctx.CWD,
		MaxConcurrency: kctx.MaxConcurrency,
	}
}

// plakarConfigFrom builds the repository configuration the worker opens, from the
// credentials that arrived over stdin.
func plakarConfigFrom(repoPath string, creds runnerCredentials, s3Insecure bool) *PlakarConfig {
	return &PlakarConfig{
		RepoPath:    repoPath,
		Passphrase:  creds.Passphrase,
		S3AccessKey: creds.S3AccessKey,
		S3SecretKey: creds.S3SecretKey,
		S3Insecure:  s3Insecure,
	}
}

// connectorConfig builds the connector configuration for a location, applying the
// database password as the standalone `password` option.
//
// The URI on the worker's command line carries the username but no password (see
// dbURI); both pinned connectors document standalone keys as overriding the
// location URI, so this is where the credential is reunited with the connection.
// Explicit --opt values still win, so nothing here can silently override an
// operator's choice.
func connectorConfig(location string, creds runnerCredentials, opts map[string]string) map[string]string {
	config := map[string]string{"location": location}
	if creds.DBPassword != "" {
		config["password"] = creds.DBPassword
	}
	for k, v := range opts {
		config[k] = v
	}
	return config
}

// runConnectorBackup runs the registered importer for sourceURI and writes one
// snapshot (with the given tags) into the kloset repository at repoPath.
func runConnectorBackup(repoPath, sourceURI string, creds runnerCredentials, s3Insecure bool, opts map[string]string, tags []string) (retErr error) {
	cfg := plakarConfigFrom(repoPath, creds, s3Insecure)
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		return err
	}
	defer closePlakarContext(kctx)

	// The host (ensurePlakarRepo) always creates the repository — with the right
	// encryption — before any runner launches, so the worker only ever OPENS it.
	// Using openRepo (not openOrCreateRepo) means a missing/unreachable repo fails
	// loudly here instead of the worker silently creating a NEW plaintext repo and
	// writing the database dump in the clear.
	repo, err := openRepo(kctx, cfg)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	imp, err := importer.NewImporter(kctx, connectorOptions(kctx), connectorConfig(sourceURI, creds, opts))
	if err != nil {
		return fmt.Errorf("creating importer for %q: %w", sourceURI, err)
	}
	defer imp.Close(kctx.Context)

	src, err := snapshot.NewSource(context.Background(), imp)
	if err != nil {
		return fmt.Errorf("creating snapshot source: %w", err)
	}

	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, &snapshot.BuilderOptions{
		Name: sourceURI,
		Tags: tags,
	})
	if err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}
	defer builder.Close()

	if err := builder.Backup(src); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	if err := builder.Commit(); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}

	fmt.Printf("%x\n", builder.Header.Identifier)
	return nil
}

// preserveOwnership records the ownership of dir and returns a function that re-applies
// it to dir and everything beneath it.
//
// The runner bypasses the database image's entrypoint so that --user root takes effect,
// which means a restore writes the data directory as root. Neo4j runs as its own user
// (uid 7474) and will not start on a database directory it cannot read, so the ownership
// the directory had before the restore has to be the ownership it has after. Until the
// entrypoint was bypassed this was correct only by accident: neo4j's entrypoint dropped
// privileges, so the restore happened to run as the right user.
//
// An empty dir disables this, and a dir that does not exist is not an error — the
// Postgres restore shares no volumes and has no data directory here.
func preserveOwnership(dir string) (func() error, error) {
	if dir == "" {
		return func() error { return nil }, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return func() error { return nil }, nil
		}
		return nil, fmt.Errorf("inspecting %s before restore: %w", dir, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Not a platform that reports uid/gid; the runner is always Linux, so this
		// only spares a developer running the worker directly on another OS.
		logrus.Debugf("ownership of %s cannot be read on this platform; leaving it alone", dir)
		return func() error { return nil }, nil
	}
	uid, gid := int(stat.Uid), int(stat.Gid)

	return func() error {
		restored := 0
		err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := os.Lstat(path)
			if err != nil {
				return err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if ok && int(st.Uid) == uid && int(st.Gid) == gid {
				return nil
			}
			if err := os.Lchown(path, uid, gid); err != nil {
				return fmt.Errorf("restoring ownership of %s: %w", path, err)
			}
			restored++
			return nil
		})
		if err != nil {
			return err
		}
		if restored > 0 {
			logrus.Infof("Restored ownership of %d path(s) under %s to %d:%d", restored, dir, uid, gid)
		}
		return nil
	}, nil
}

// Neo4jMigration asks for `neo4j-admin database migrate --to-format=<Format>` to
// run against Database once the restore has written the store.
//
// It exists because a format migration has to happen inside the same offline
// window as the load: neo4j-admin will not migrate a store the server has opened,
// and by the time the orchestrator's deferred StartServices("database") has run,
// the window is gone. main could run it through `docker compose exec` because its
// Community path only SIGSTOPped the neo4j process and left the container up; the
// runner path stops the container, so the migration belongs here, next to the
// load, in the container that has both the data volume and neo4j-admin.
type Neo4jMigration struct {
	// Format is the --to-format value ("block"); empty means no migration.
	Format string
	// Database is the database to migrate.
	Database string
}

// Requested reports whether a migration was asked for.
func (m Neo4jMigration) Requested() bool { return m.Format != "" }

// run executes the migration with neo4j-admin from binDir (or $PATH when empty).
func (m Neo4jMigration) run(binDir string) error {
	if !m.Requested() {
		return nil
	}
	if m.Database == "" {
		return fmt.Errorf("--migrate-format=%s requires --migrate-database", m.Format)
	}
	bin := "neo4j-admin"
	if binDir != "" {
		bin = filepath.Join(binDir, bin)
	}
	logrus.Infof("Migrating %s to --to-format=%s...", m.Database, m.Format)
	if _, err := runCapture(runnerTimeout(), bin, []string{"database", "migrate", "--to-format=" + m.Format, m.Database}, ""); err != nil {
		return fmt.Errorf("migrating %s to format %s: %w", m.Database, m.Format, err)
	}
	logrus.Infof("Migrated %s to format %s", m.Database, m.Format)
	return nil
}

// runConnectorRestore loads the snapshot and drives the registered exporter for destURI.
func runConnectorRestore(repoPath, destURI, snapHex string, creds runnerCredentials, s3Insecure bool,
	preserveOwnerDir string, opts map[string]string, migrate Neo4jMigration) error {
	// Captured before the export so it reflects the ownership the database had, not
	// whatever the restore leaves behind.
	restoreOwnership, err := preserveOwnership(preserveOwnerDir)
	if err != nil {
		return err
	}

	cfg := plakarConfigFrom(repoPath, creds, s3Insecure)
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		return err
	}
	defer closePlakarContext(kctx)

	repo, err := openRepo(kctx, cfg)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	mac, err := parseMAC(snapHex)
	if err != nil {
		return err
	}
	snap, err := snapshot.Load(repo, mac)
	if err != nil {
		return fmt.Errorf("loading snapshot %s: %w", snapHex, err)
	}

	exp, err := exporter.NewExporter(kctx, connectorOptions(kctx), connectorConfig(destURI, creds, opts))
	if err != nil {
		return fmt.Errorf("creating exporter for %q: %w", destURI, err)
	}

	return exportWithOwnershipRestored(
		func() error {
			if err := snap.Export(exp, "/", &snapshot.ExportOptions{SkipPermissions: true}); err != nil {
				return fmt.Errorf("restore failed: %w", err)
			}
			return nil
		},
		func() error {
			if err := exp.Close(kctx.Context); err != nil {
				return fmt.Errorf("closing exporter for %q: %w", destURI, err)
			}
			return nil
		},
		func() error { return migrate.run(opts["neo4j_bin_dir"]) },
		restoreOwnership,
	)
}

// exportWithOwnershipRestored drives a snapshot export, closes the exporter, runs
// afterRestore, and restores the data directory's ownership. The close and the
// chown happen on EVERY exit path, including a failed export.
//
// A failed restore needs the chown at least as much as a successful one does. The
// runner bypasses the database image's entrypoint so --user root takes effect, so
// by the time an export fails neo4j-admin may already have written into the shared
// /data as real root. Returning early from there left the store root-owned, and
// the orchestrator's deferred StartServices("database") then booted Neo4j as uid
// 7474 on a directory it cannot read: a failed restore became a dead deployment.
//
// Order matters as much as coverage. The exporter is what drives neo4j-admin, so
// anything it writes while closing has to be chowned too, as does anything
// afterRestore (a format migration) writes — hence close, then afterRestore, then
// ownership last (the deferred calls run in reverse registration order).
// afterRestore is the remainder of the offline window, so it is skipped when the
// restore itself failed. The first error wins, so a genuine restore failure is
// never masked by a clean-up error.
func exportWithOwnershipRestored(export, closeExporter, afterRestore, restoreOwnership func() error) (retErr error) {
	defer func() {
		if err := restoreOwnership(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	defer func() {
		if err := closeExporter(); err != nil && retErr == nil {
			retErr = err
		}
		if retErr != nil {
			return // nothing was restored; there is nothing to migrate
		}
		if err := afterRestore(); err != nil {
			retErr = err
		}
	}()
	return export()
}

func parseMAC(s string) (objects.MAC, error) {
	var mac objects.MAC
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return mac, fmt.Errorf("invalid snapshot id %q: %w", s, err)
	}
	if len(b) != len(mac) {
		return mac, fmt.Errorf("invalid snapshot id length %d (want %d)", len(b), len(mac))
	}
	copy(mac[:], b)
	return mac, nil
}
