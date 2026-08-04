@AGENTS.md

## Active Technologies
- Go 1.25.0 (pinned in `go.mod`) + cobra (CLI), viper (config), logrus (logging), minio-go v7 (S3) — all existing; **no new dependencies** (so no `vendorHash` update needed) (005-restore-latest-backup)
- Local backup directory (`Configuration.BackupDir`) and S3 bucket/prefix via the existing `S3Client`; plakar repository untouched (005-restore-latest-backup)

## Recent Changes
- 005-restore-latest-backup: Added Go 1.25.0 (pinned in `go.mod`) + cobra (CLI), viper (config), logrus (logging), minio-go v7 (S3) — all existing; **no new dependencies** (so no `vendorHash` update needed)
