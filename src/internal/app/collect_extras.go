package app

import (
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

// backupCollectorName is the manifest identifier of the include-backup
// collector (specs/003-collect-tool/data-model.md, CollectorResult.Name).
const backupCollectorName = "backup"

// backupArchiveGlob matches the tarball archives CreateBackup writes into the
// backup directory (see generateBackupFilename).
const backupArchiveGlob = "infrahub_backup_*.tar.gz"

// backupRunner produces a backup and returns the path of the created archive
// (empty when the artifact cannot be located). The production runner is
// runStandardBackup; unit tests inject fakes.
type backupRunner func(cc *collectContext) (string, error)

// includeBackupCollector returns the opt-in --include-backup collector
// (FR-014, research R10). It must stay last in the run plan: the delegated
// backup inherits the standard backup behavior, which may stop and restart
// application containers, so every read-only collector stages its files
// first and a backup failure cannot taint the diagnostics (US3 scenario 2).
// The produced archive stays a standalone file in the standard backup
// directory — referenced by the manifest, not embedded in the bundle.
func includeBackupCollector(run backupRunner) collector {
	return collector{
		name: backupCollectorName,
		skip: func(cc *collectContext) (bool, string) {
			return !cc.opts.IncludeBackup, "not requested"
		},
		run: func(cc *collectContext) error {
			artifact, err := run(cc)
			if err != nil {
				return fmt.Errorf("backup failed: %w", err)
			}
			cc.setArtifact(artifact)
			return nil
		},
	}
}

// runStandardBackup delegates to the existing CreateBackup unmodified, with
// the non-interactive `infrahub-backup create` defaults: no --force (the
// running-tasks safety check is preserved, Principle II), all Neo4j metadata,
// task-manager database included, no S3 upload, no sleep, no redaction, no
// encryption (research R10). CreateBackup does not return the path it wrote,
// so the new archive is identified by diffing the backup directory listing
// around the call.
func runStandardBackup(cc *collectContext) (string, error) {
	backupDir := cc.iops.config.BackupDir

	before, err := listBackupArchives(backupDir)
	if err != nil {
		return "", err
	}

	if err := cc.iops.CreateBackup(false, "all", false, false, false, 0, false, false, ""); err != nil {
		return "", err
	}

	after, err := listBackupArchives(backupDir)
	if err != nil {
		return "", err
	}

	artifact := newestNewArchive(before, after)
	if artifact == "" {
		// Nothing to reference (e.g. a non-tarball backend wrote elsewhere).
		// The backup itself succeeded, so this is a warning, not a failure.
		logrus.Warnf("Backup completed but no new archive was found in %s; the manifest entry will carry no artifact path", backupDir)
		return "", nil
	}

	if abs, absErr := filepath.Abs(artifact); absErr == nil {
		artifact = abs
	}
	return artifact, nil
}

// listBackupArchives returns the backup archives currently present in dir.
// A missing directory yields an empty list (CreateBackup creates it).
func listBackupArchives(dir string) ([]string, error) {
	archives, err := filepath.Glob(filepath.Join(dir, backupArchiveGlob))
	if err != nil {
		return nil, fmt.Errorf("failed to list backup archives in %s: %w", dir, err)
	}
	return archives, nil
}

// newestNewArchive returns the newest archive present in after but absent
// from before. Archive names embed a YYYYMMDD_HHMMSS timestamp, so within one
// directory the lexically greatest new path is the most recent one.
func newestNewArchive(before, after []string) string {
	seen := make(map[string]struct{}, len(before))
	for _, path := range before {
		seen[path] = struct{}{}
	}

	newest := ""
	for _, path := range after {
		if _, ok := seen[path]; ok {
			continue
		}
		if path > newest {
			newest = path
		}
	}
	return newest
}
