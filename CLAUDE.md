@AGENTS.md

## Active Technologies
- Go 1.25.0 (pinned in `go.mod`), `CGO_ENABLED=0` + cobra (CLI), viper (config/env binding, prefix `INFRAHUB_`), logrus (logging), `minio-go/v7` (S3 — already a direct dependency); kloset (Plakar core) for the deferred P3 slice. **No new dependencies.** (004-backup-retention-policy)
- local filesystem (`Configuration.BackupDir`, default `./infrahub_backups`); S3-compatible object storage via existing `S3Client` (`src/internal/app/s3.go`); Plakar repository (P3 only) (004-backup-retention-policy)

## Recent Changes
- 004-backup-retention-policy: Added Go 1.25.0 (pinned in `go.mod`), `CGO_ENABLED=0` + cobra (CLI), viper (config/env binding, prefix `INFRAHUB_`), logrus (logging), `minio-go/v7` (S3 — already a direct dependency); kloset (Plakar core) for the deferred P3 slice. **No new dependencies.**
