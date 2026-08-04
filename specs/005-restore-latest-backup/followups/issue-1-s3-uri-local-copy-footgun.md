# Follow-up issue draft 1

Filed as <https://github.com/opsmill/infrahub-backup/issues/158>.

Labels applied: `type: bug`, `claude-code-assisted`.

## Title

`restore s3://…` truncates and then deletes a same-named local backup archive

## Body

### Summary

Restoring an exact remote archive by its positional `s3://bucket/key` URI downloads the
object to `BackupDir/<basename>` and deletes that path when the restore finishes. When the
host already holds a local archive of the same name, the download overwrites it and the
cleanup then removes it — the operator's local backup copy is silently gone, and the run
reports success.

Pre-existing behavior, unchanged by `restore --latest` (spec
`005-restore-latest-backup`, research decision D3 scoped it out deliberately). Filed so it
is not lost.

### Mechanism

1. `downloadBackupFromS3` (`src/internal/app/backup_s3.go:29`) computes the download target
   as `filepath.Join(iops.config.BackupDir, filepath.Base(key))` — the archive's own name,
   inside the backup directory (`backup_s3.go:53-54`).
2. `S3Client.Download` (`src/internal/app/s3.go:247`) opens that path with `os.Create`,
   which truncates an existing file.
3. `RestoreBackup` (`src/internal/app/backup.go:305-314`) sets
   `actualBackupFile = downloadedPath` and registers
   `defer os.Remove(actualBackupFile)`, so the path is deleted after the restore.

### When it bites

`create --s3-upload --s3-keep-local` is exactly the flow that puts the same archive name in
both the bucket and the local backup directory. On such a host:

```bash
infrahub-backup create --s3-upload --s3-keep-local          # local + remote copy
infrahub-backup restore s3://my-backups/infrahub_backup_20260804_030000.tar.gz
# the local infrahub_backup_20260804_030000.tar.gz is now gone
```

The archive is still in the bucket, so this is a loss of a local copy rather than of the
only copy — but the local copy is the one an operator reaches for when the bucket is
unreachable, and nothing in the output says it was consumed.

### Fix direction

`restore --latest --s3` already avoids this, and its shape is the fix sketch. That leg
(`src/internal/app/restore_latest.go`, `downloadLatestS3Backup`) downloads through
`os.CreateTemp(BackupDir, "restore-latest-*.download")`:

- the name is collision-proof and reserved on disk, so it can never be an existing
  archive's name and two concurrent restores cannot download over each other;
- the name deliberately cannot match `backupNamePattern`, so a temp file left behind by a
  crash is invisible to retention and to a later `--latest` — it can neither be selected
  nor pruned as if it were a backup;
- cleanup is deferred, so it also covers a failed download and a failed restore.

Applying the same treatment to `downloadBackupFromS3` — download to a reserved temporary
name, restore from it, remove it — closes this without changing the URI form's user-facing
behavior. Alternatively, refuse to overwrite an existing file at the target path, which is
a smaller change but leaves the operator to clean up by hand.

### Notes

- No behavior in `restore --latest` / `restore --latest --s3` is affected; those already
  take the safe path.
- Worth an e2e assertion alongside the fix: after a positional-URI restore, a pre-existing
  local archive of the same name still exists and is byte-identical. The `--latest --s3`
  leg already has that assertion in `tests/e2e/test_docker_s3.py`.
