package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
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
	backupCmd := &cobra.Command{
		Use:          "backup <repo> <source-uri>",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runConnectorBackup(args[0], args[1], parseKV(backupOpts), tags)
		},
	}
	backupCmd.Flags().StringArrayVar(&backupOpts, "opt", nil, "connector option key=value (repeatable)")
	backupCmd.Flags().StringArrayVar(&tags, "tag", nil, "snapshot tag key=value (repeatable)")

	var restoreOpts []string
	restoreCmd := &cobra.Command{
		Use:          "restore <repo> <dest-uri> <snapshot-hex>",
		Args:         cobra.ExactArgs(3),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runConnectorRestore(args[0], args[1], args[2], parseKV(restoreOpts))
		},
	}
	restoreCmd.Flags().StringArrayVar(&restoreOpts, "opt", nil, "connector option key=value (repeatable)")

	cmd.AddCommand(backupCmd, restoreCmd)
	return cmd
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
func runConnectorBackup(repoPath, sourceURI string, opts map[string]string, tags []string) (retErr error) {
	cfg := &PlakarConfig{RepoPath: repoPath}
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		return err
	}
	defer closePlakarContext(kctx)

	repo, err := openOrCreateRepo(kctx, cfg)
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

// runConnectorRestore loads the snapshot and drives the registered exporter for destURI.
func runConnectorRestore(repoPath, destURI, snapHex string, opts map[string]string) error {
	cfg := &PlakarConfig{RepoPath: repoPath}
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
	defer exp.Close(kctx.Context)

	if err := snap.Export(exp, "/", &snapshot.ExportOptions{SkipPermissions: true}); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}
	return nil
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
