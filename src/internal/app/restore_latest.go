package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// encryptedArchiveSuffix is what the encryption path appends to a backup archive's name,
// and the optional half of backupNamePattern that marks a listed archive as encrypted.
const encryptedArchiveSuffix = ".enc"

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
	if s3 {
		return errors.New("restore --latest --s3 is not implemented yet: pass an archive's s3://bucket/key URI as the argument to restore that exact remote archive")
	}

	return restoreLatestFrom(context.Background(), newLocalLocation(iops.config.BackupDir), decryptKey, func(ref backupRef) error {
		// A local archive is already where the restore path reads it from. The ref
		// carries a base name, and joining it under the pool's own directory — the
		// directory that was listed — is what turns it back into a path.
		return iops.RestoreBackup(filepath.Join(iops.config.BackupDir, ref.Name),
			excludeTaskManager, restoreMigrateFormat, sleepDuration, decryptKey, force, resetDeploymentID)
	})
}
