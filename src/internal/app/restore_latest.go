package app

import (
	"context"
	"fmt"
)

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
