# Follow-up issue draft 4

Ready to file on `opsmill/infrahub-backup`. Not yet filed — awaiting approval.

Suggested labels: `bug`, `backup`, `data-integrity`

Found by the code-quality review of the `005-restore-latest-backup` change set, not by a
task in it. Pre-existing in `RestoreBackup`; recorded here because `restore --latest` is
what makes it reachable without an operator ever naming the encrypted archive.

## Title

Restoring an encrypted archive overwrites and then deletes a same-timestamp plain archive

## Summary

When `restore` decrypts an archive, it writes the plaintext to the encrypted archive's own
path minus the `.enc` suffix, then deletes that path when the restore finishes. If a plain
archive with the same timestamp already sits in the backup directory, it is silently
overwritten and then removed. Two backups go in, one comes out, and nothing reports a loss.

This is pre-existing behavior of the decrypt path, reachable today with
`restore infrahub_backup_<ts>.tar.gz.enc --decrypt-key key.pem`. It matters more now:
`restore --latest` picks the archive itself, and on a timestamp tie it deliberately prefers
the `.enc` member, so an operator can hit this without naming — or even knowing about —
the encrypted archive.

## Mechanism

In `RestoreBackup` (`src/internal/app/backup.go`):

1. `decryptedPath := strings.TrimSuffix(actualBackupFile, ".enc")` — for
   `<dir>/infrahub_backup_20260801_030000.tar.gz.enc` this is exactly
   `<dir>/infrahub_backup_20260801_030000.tar.gz`, a valid archive name in the same pool.
2. `DecryptFile(actualBackupFile, decryptedPath, privKey)` writes there, **overwriting** an
   existing plain archive at that path.
3. `if IsS3URI(backupFile) { os.Remove(actualBackupFile) }` — the guard that would spare a
   local file does not apply, because a local restore's `backupFile` is a path, not a URI.
4. `actualBackupFile = decryptedPath` followed by `defer os.Remove(actualBackupFile)` —
   **deletes** the plain archive when the restore completes.

The `--latest` selection rule routes into it: `sortBackupRefsNewestFirst`
(`src/internal/app/retention.go`) breaks a timestamp tie with `strings.Compare(b.Name, a.Name)`,
i.e. name descending, and `"…tar.gz.enc"` sorts above `"…tar.gz"`. That tiebreak is
intentional and is pinned by a test in `restore_latest_test.go`.

## Reproduction

```bash
# A backup directory holding both members of a same-timestamp pair.
ls "$BACKUP_DIR"
# infrahub_backup_20260801_030000.tar.gz
# infrahub_backup_20260801_030000.tar.gz.enc

sha256sum "$BACKUP_DIR/infrahub_backup_20260801_030000.tar.gz"   # record it

bin/infrahub-backup restore --latest --decrypt-key /path/to/backup.key

ls "$BACKUP_DIR"
# infrahub_backup_20260801_030000.tar.gz.enc      <- survives
# infrahub_backup_20260801_030000.tar.gz          <- GONE
```

Expected: both archives still present, unmodified. Actual: the plain archive is overwritten
during decryption and deleted afterwards.

## How the pair arises

`create --encrypt-key` removes the plain archive after encrypting it
(`src/internal/app/backup.go`), so the normal create flow does not leave both. Reaching the
pair takes a mixed or manual history — encrypting an archive out of band, restoring a plain
archive alongside an encrypted one, or copying archives between hosts. Low likelihood,
total loss of one archive when it happens, and no warning either way.

## Fix direction

Decrypt to a path that cannot collide with a pool member, exactly as this feature's
`--latest --s3` leg already does for its download: `os.CreateTemp` with a pattern that
cannot match `backupNamePattern` (see `downloadLatestS3Backup` in
`src/internal/app/restore_latest.go`), then remove that temp file on every exit path. That
removes both the overwrite and the delete, and makes the `.enc`-preferring tiebreak safe.

A narrower alternative — refuse to decrypt when `decryptedPath` already exists — protects
the data but fails a restore that should have worked, so the temp-path fix is preferable.

## Related

- Draft 1 (`issue-1-s3-uri-local-copy-footgun.md`) is the same class of defect on the
  positional `s3://` URI path: a download that lands on an archive's own name and is then
  deleted. Both are cases of the restore path writing into the pool it reads from, and a
  single temp-path convention for restore inputs would close both.
