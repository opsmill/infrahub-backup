# CLI Contract: `infrahub-collect create --include-task-manager-storage`

**Feature**: [spec.md](../spec.md) (FR-004, FR-009, FR-015)

## New flag on `infrahub-collect create`

| Flag | Default | Environment variable | Effect |
|---|---|---|---|
| `--include-task-manager-storage` | `false` | `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE` | Also collect the storage breakdown of the task manager database into `task-manager-db/storage-breakdown.json`. Each table's breakdown stops after 5 minutes. |

- Registration follows `--include-backup` in [`main.go`](../../../src/cmd/infrahub-collect/main.go): a `BoolVar` on `createCmd`, bound with `viper.BindPFlag("include-task-manager-storage", ...)`, read with `viper.GetBool` into `CollectOptions.IncludeTaskManagerStorage`. The flag takes precedence over the environment variable, through the existing `INFRAHUB_` prefix binding in `ConfigureRootCommand`.
- Help text: `Also collect the storage breakdown of the task manager database (reads every row of its run, state, log, event and artifact tables; up to 5 minutes per table)`.
- `infrahub-backup` and `infrahub-taskmanager` do not change.

## No flag for the always-collected figures

`task-manager-db/storage.json` is collected on every run of `create`. There is no flag to turn it off, as for the other always-on diagnostics.

## Exit codes

Unchanged. `create` exits 0 whenever the archive is produced, including when either new collector is `failed` or `skipped`.

## Test-only environment variable

| Variable | Read by | Effect |
|---|---|---|
| `INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT` | the query runner shared by both collectors, with `os.Getenv` | A Go duration, for example `5s`. The storage query's limit becomes the smaller of 60 seconds and this value, and each breakdown table's limit becomes the smaller of 5 minutes and this value. A value that does not parse or is not positive is ignored with a warning. |

This variable exists so the end-to-end test can reach both limits in seconds (FR-009). It is not documented for users and can never raise a limit.

## Documentation to change (FR-015)

| Page | Change |
|---|---|
| [`docs/docs/collect/index.mdx`](../../../docs/docs/collect/index.mdx), "What a bundle contains" | Add the `task-manager-db/` directory and both files. |
| [`docs/docs/collect/create.mdx`](../../../docs/docs/collect/create.mdx) | Add a section on collecting the storage breakdown when support asks for it. It says: the breakdown can take up to 5 minutes for each of the seven tables; a table that reaches the limit is recorded as failed and the other tables are still collected; do not use the flag while the task manager is being upgraded; and to stop the tool's query at once, run `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'infrahub-collect'` in the database. It also says that during an upgrade the always-collected storage figures can be missing, with the reason `timed out after 60s`. |
| [`docs/docs/reference/commands.mdx`](../../../docs/docs/reference/commands.mdx) | Add the flag to the `create` flag table and the bundle contents table. |
| [`docs/docs/reference/configuration.mdx`](../../../docs/docs/reference/configuration.mdx) | Add the flag and its environment variable. |
