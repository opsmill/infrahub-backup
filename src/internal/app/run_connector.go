package app

import (
	"context"
	"encoding/hex"
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
	var backupPassphraseStdin bool
	backupCmd := &cobra.Command{
		Use:          "backup <repo> <source-uri>",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			passphrase, err := readPassphraseStdinIf(backupPassphraseStdin)
			if err != nil {
				return err
			}
			return runConnectorBackup(args[0], args[1], passphrase, parseKV(backupOpts), tags)
		},
	}
	backupCmd.Flags().StringArrayVar(&backupOpts, "opt", nil, "connector option key=value (repeatable)")
	backupCmd.Flags().StringArrayVar(&tags, "tag", nil, "snapshot tag key=value (repeatable)")
	addPassphraseStdinFlag(backupCmd, &backupPassphraseStdin)

	var restoreOpts []string
	var restorePassphraseStdin bool
	var restorePreserveOwner string
	restoreCmd := &cobra.Command{
		Use:          "restore <repo> <dest-uri> <snapshot-hex>",
		Args:         cobra.ExactArgs(3),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			passphrase, err := readPassphraseStdinIf(restorePassphraseStdin)
			if err != nil {
				return err
			}
			return runConnectorRestore(args[0], args[1], args[2], passphrase, restorePreserveOwner, parseKV(restoreOpts))
		},
	}
	restoreCmd.Flags().StringArrayVar(&restoreOpts, "opt", nil, "connector option key=value (repeatable)")
	restoreCmd.Flags().StringVar(&restorePreserveOwner, "preserve-owner", "",
		"directory whose ownership must survive the restore (the DB data dir, written as root)")
	addPassphraseStdinFlag(restoreCmd, &restorePassphraseStdin)

	// launch: exercise the co-located runner launcher through the tool (testing the
	// orchestration path; the create flow will call LaunchComposeBackup directly).
	var launchOpts, launchTags []string
	var launchVolumes, launchPassphraseStdin bool
	launchCmd := &cobra.Command{
		Use:          "launch <project> <db-service> <repo> <source-uri>",
		Args:         cobra.ExactArgs(4),
		Hidden:       true,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			passphrase, err := readPassphraseStdinIf(launchPassphraseStdin)
			if err != nil {
				return err
			}
			snap, err := LaunchComposeBackup(args[0], args[1], args[2], args[3], passphrase, parseKV(launchOpts), launchTags, launchVolumes)
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
	addPassphraseStdinFlag(launchCmd, &launchPassphraseStdin)

	cmd.AddCommand(backupCmd, restoreCmd, launchCmd)
	return cmd
}

// addPassphraseStdinFlag registers the shared --passphrase-stdin flag on a
// subcommand, binding it to target. Centralized so the flag name and help text
// stay identical across the backup/restore/launch subcommands.
func addPassphraseStdinFlag(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVar(target, "passphrase-stdin", false, "read the repository passphrase from stdin (one line)")
}

// readPassphraseStdinIf reads one line from stdin as the repository passphrase
// when enabled. The orchestrator pipes it in via `docker run -i`, so it never
// appears on the worker's command line or environment (FR-007). The trailing
// CR/LF is stripped; interior characters are preserved.
func readPassphraseStdinIf(enabled bool) (string, error) {
	if !enabled {
		return "", nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading passphrase from stdin: %w", err)
	}
	return firstLine(string(data)), nil
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

// runConnectorBackup runs the registered importer for sourceURI and writes one
// snapshot (with the given tags) into the kloset repository at repoPath.
func runConnectorBackup(repoPath, sourceURI, passphrase string, opts map[string]string, tags []string) (retErr error) {
	cfg := &PlakarConfig{RepoPath: repoPath, Passphrase: passphrase}
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

	config := map[string]string{"location": sourceURI}
	for k, v := range opts {
		config[k] = v
	}

	imp, err := importer.NewImporter(kctx, connectorOptions(kctx), config)
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

// runConnectorRestore loads the snapshot and drives the registered exporter for destURI.
func runConnectorRestore(repoPath, destURI, snapHex, passphrase, preserveOwnerDir string, opts map[string]string) error {
	// Captured before the export so it reflects the ownership the database had, not
	// whatever the restore leaves behind.
	restoreOwnership, err := preserveOwnership(preserveOwnerDir)
	if err != nil {
		return err
	}

	cfg := &PlakarConfig{RepoPath: repoPath, Passphrase: passphrase}
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

	config := map[string]string{"location": destURI}
	for k, v := range opts {
		config[k] = v
	}

	exp, err := exporter.NewExporter(kctx, connectorOptions(kctx), config)
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
		restoreOwnership,
	)
}

// exportWithOwnershipRestored drives a snapshot export, then closes the exporter,
// then restores the data directory's ownership — the last two on EVERY exit path,
// including a failed export.
//
// A failed restore needs the chown at least as much as a successful one does. The
// runner bypasses the database image's entrypoint so --user root takes effect, so
// by the time an export fails neo4j-admin may already have written into the shared
// /data as real root. Returning early from there left the store root-owned, and
// the orchestrator's deferred StartServices("database") then booted Neo4j as uid
// 7474 on a directory it cannot read: a failed restore became a dead deployment.
//
// Order matters as much as coverage. The exporter is what drives neo4j-admin, so
// anything it writes while closing has to be chowned too — hence close first,
// ownership last (the deferred calls run in reverse registration order). The first
// error wins, so a genuine restore failure is never masked by a clean-up error.
func exportWithOwnershipRestored(export, closeExporter, restoreOwnership func() error) (retErr error) {
	defer func() {
		if err := restoreOwnership(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	defer func() {
		if err := closeExporter(); err != nil && retErr == nil {
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
