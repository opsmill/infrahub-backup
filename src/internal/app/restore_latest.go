package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// encryptedArchiveSuffix is what the encryption path appends to a backup archive's name,
// and the optional half of backupNamePattern that marks a listed archive as encrypted.
const encryptedArchiveSuffix = ".enc"

// s3RestoreTempPattern is the os.CreateTemp pattern the S3 leg downloads through.
//
// The name it produces can never match backupNamePattern, and that is the point: the
// download is invisible to the pool, so a temp file left behind by a crash can neither be
// selected by a later --latest run nor deleted by retention nor mistaken for an archive.
// It also keeps the download away from BackupDir/<archive-name>, which is what makes a
// local archive sharing the selected object's name — the state `create --s3-upload`
// leaves on every host that keeps its local copy — neither overwritten nor deleted by a
// restore (contracts/cli.md "Local-copy safety").
const s3RestoreTempPattern = "restore-latest-*.download"

// s3RestoreDownloadTimeout bounds the download of the selected archive. It matches the
// allowance the positional s3:// URI restore path already gives a download, and exists so
// that an endpoint which accepts the connection but never answers cannot hang a scheduled
// restore forever.
const s3RestoreDownloadTimeout = 30 * time.Minute

// s3RestoreClient is the slice of S3Client the `--latest --s3` leg needs on top of the
// listing its location performs: the key an archive's base name maps to, and the download
// of that key.
//
// The leg depends on this interface rather than on the concrete client so the wiring
// between them is exercisable without a bucket — above all that the download lands on a
// temporary path and never on the archive's own name, which against real S3 would look
// like a perfectly successful restore.
type s3RestoreClient interface {
	buildS3Key(name string) string
	Download(ctx context.Context, s3Key, localPath string) error
}

var _ s3RestoreClient = (*S3Client)(nil)

// resolveLatestBackup returns the most recent backup at one storage location.
//
// "Most recent" is exactly what retention means by it, and deliberately so: the
// location's listing already admits only names matching backupNamePattern, and
// sortBackupRefsNewestFirst is the one ranking implementation both features share
// (embedded timestamp descending, ties broken by name descending). Sharing it makes
// "the backup retention always keeps" and "the backup --latest restores" the same
// archive by construction rather than by convention (FR-005).
//
// An empty pool is an error rather than an empty selection: there is nothing to
// restore, and a caller that read it as a no-op would leave a scheduled sync silently
// doing nothing on the runs nobody watches (FR-008). A location that cannot be listed
// at all — a mistyped backup directory, an unreachable bucket — reports its own
// failure with the location it happened at added.
func resolveLatestBackup(ctx context.Context, loc storageLocation) (backupRef, error) {
	refs, err := loc.List(ctx)
	if err != nil {
		return backupRef{}, fmt.Errorf("failed to resolve the latest backup at %s: %w", loc.Name(), err)
	}

	if len(refs) == 0 {
		return backupRef{}, fmt.Errorf("no backups found at %s: nothing for --latest to restore", loc.Name())
	}

	// The listing is this function's own slice, so ranking it in place cannot disturb
	// a caller's ordering.
	sortBackupRefsNewestFirst(refs)

	return refs[0], nil
}

// restoreLatestFrom performs the whole selection half of `restore --latest` at one
// storage location and hands the resolved archive to deliver, the only pool-specific
// step: a local archive is already readable on this host, an S3 object has to be
// downloaded first.
//
// Everything that can refuse the run happens before deliver is called, and therefore
// before any download, before the --sleep wait, and before any container is touched
// (FR-007). Refusing is the point: falling back to the newest archive that happens to
// be readable would leave a staging deployment quietly holding stale data, which is
// worse than a job that visibly failed.
func restoreLatestFrom(ctx context.Context, loc storageLocation, decryptKey string, deliver func(ref backupRef) error) error {
	ref, err := resolveLatestBackup(ctx, loc)
	if err != nil {
		return err
	}

	// The name is all there is to go on, and it has to be: FR-007 requires the refusal
	// before the archive is downloaded, so its content cannot be inspected yet. The name
	// is trustworthy here because the pool was filtered by backupNamePattern, whose
	// optional .enc half is the same suffix the encryption path writes. RestoreBackup
	// still content-checks the archive afterwards, so a mislabeled file is caught too.
	if strings.HasSuffix(ref.Name, encryptedArchiveSuffix) && decryptKey == "" {
		return fmt.Errorf("latest backup %s at %s is encrypted: pass --decrypt-key with its private key PEM file; --latest never falls back to an older archive", ref.Name, loc.Name())
	}

	// The one line a scheduled restore is audited from after the fact: what was chosen,
	// and out of which pool (FR-009, SC-003).
	logrus.Infof("Restoring latest backup %s from %s", ref.Name, loc.Name())

	return deliver(ref)
}

// downloadLatestS3Backup fetches the selected object into dir under a name that is not a
// backup archive name, hands that path to restore, and removes it again on the way out.
//
// The temporary path is the whole safety of this leg. Downloading to dir/<archive-name>
// would truncate a local archive of the same name and then let the restore path delete it
// — and `create --s3-upload` keeping its local copy makes that collision the expected
// state of this flow, not a rare accident, so a nightly sync nobody watches would quietly
// consume the host's own backups. The download therefore goes to a reserved temporary name
// and the local pool is left exactly as it was found.
//
// Cleanup is deferred so it also covers a failed download and a failed restore: an
// abandoned download would otherwise hold a full archive's worth of disk in the backup
// directory for nobody.
func downloadLatestS3Backup(ctx context.Context, client s3RestoreClient, dir string, ref backupRef, restore func(path string) error) error {
	// An S3 restore may well run on a host that has never taken a backup, so the
	// directory the download lands in is created rather than assumed — the same
	// allowance the positional s3:// URI path makes.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory %s: %w", dir, err)
	}

	// Created rather than merely named: the name is reserved on disk, so two restores
	// sharing a backup directory cannot download over each other.
	file, err := os.CreateTemp(dir, s3RestoreTempPattern)
	if err != nil {
		return fmt.Errorf("failed to create a temporary download path in %s: %w", dir, err)
	}
	tempPath := file.Name()

	// The download opens the path itself, so this handle has no further use; the reserved
	// name stays on disk.
	if err := file.Close(); err != nil {
		os.Remove(tempPath)

		return fmt.Errorf("failed to prepare the temporary download path %s: %w", tempPath, err)
	}

	defer func() {
		// The restore's own outcome is what the caller has to see, so a cleanup failure is
		// reported rather than returned. It cannot pollute the pool either way: the name
		// is not a backup archive name.
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logrus.Warnf("Failed to remove the temporary download %s: %v", tempPath, err)
		}
	}()

	downloadCtx, cancel := context.WithTimeout(ctx, s3RestoreDownloadTimeout)
	downloadErr := client.Download(downloadCtx, client.buildS3Key(ref.Name), tempPath)
	cancel()
	if downloadErr != nil {
		return fmt.Errorf("failed to download the latest backup %s: %w", ref.Name, downloadErr)
	}

	return restore(tempPath)
}

// RestoreLatestBackup restores the most recent backup in the configured pool without an
// archive being named — the primitive an unattended, scheduled restore needs, because a
// scheduled job cannot know a filename in advance.
//
// Exactly one pool is consulted per invocation: the local backup directory, or the
// configured S3 bucket/prefix when s3 is set. The two are never merged and there is no
// fallback between them, so a run that asked for S3 can never restore a local archive,
// nor the other way round (FR-006). Every other parameter is passed through to
// RestoreBackup untouched: this entry point only chooses the archive, it does not change
// how one is restored, so metadata validation, checksums, version compatibility, and the
// container stop/start guarantees are the existing ones (constitution II).
func (iops *InfrahubOps) RestoreLatestBackup(s3 bool, excludeTaskManager bool, restoreMigrateFormat bool, sleepDuration time.Duration, decryptKey string, force bool, resetDeploymentID bool) error {
	ctx := context.Background()

	// The one restore this entry point performs, wherever the archive came from: every
	// parameter travels through untouched, so both legs inherit the same validation,
	// decryption, and container guarantees.
	restore := func(path string) error {
		return iops.RestoreBackup(path,
			excludeTaskManager, restoreMigrateFormat, sleepDuration, decryptKey, force, resetDeploymentID)
	}

	if s3 {
		// Built exactly as `prune --s3` builds its S3 leg, so a configuration that can be
		// pruned can be restored from and both report the same location.
		if err := iops.config.S3.ValidateConfig(); err != nil {
			return err
		}

		client, err := NewS3Client(iops.config.S3)
		if err != nil {
			return fmt.Errorf("failed to create S3 client for restore: %w", err)
		}

		// One client performs both the listing and the download, so the object that is
		// downloaded is necessarily the one that was selected: there is no second
		// configuration and no URI round-trip that could resolve a different bucket,
		// prefix, or endpoint in between.
		return restoreLatestFrom(ctx, newS3Location(client), decryptKey, func(ref backupRef) error {
			return downloadLatestS3Backup(ctx, client, iops.config.BackupDir, ref, restore)
		})
	}

	return restoreLatestFrom(ctx, newLocalLocation(iops.config.BackupDir), decryptKey, func(ref backupRef) error {
		// A local archive is already where the restore path reads it from. The ref
		// carries a base name, and joining it under the pool's own directory — the
		// directory that was listed — is what turns it back into a path.
		return restore(filepath.Join(iops.config.BackupDir, ref.Name))
	})
}
