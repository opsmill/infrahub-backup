# Follow-up issue draft 3

Filed as <https://github.com/opsmill/infrahub-backup/issues/159>.

Labels applied: `type: bug`, `claude-code-assisted`.

## Title

`restore`'s `INFRAHUB_DECRYPT_KEY` and `INFRAHUB_RESET_DEPLOYMENT_ID` are bound but never read

## Body

### Summary

`restoreCmd` binds `--decrypt-key` and `--reset-deployment-id` to viper, but its `RunE` reads
the local flag variables instead of viper. The bindings are therefore dead: setting
`INFRAHUB_DECRYPT_KEY` or `INFRAHUB_RESET_DEPLOYMENT_ID` has no effect on a restore, even
though the binding implies it should.

Pre-existing. `restore --latest` (spec `005-restore-latest-backup`) inherits the same
behavior, because it passes the same local variables through.

### Details

In `src/cmd/infrahub-backup/main.go`:

```go
viper.BindPFlag("decrypt-key", restoreCmd.Flags().Lookup("decrypt-key"))
viper.BindPFlag("reset-deployment-id", restoreCmd.Flags().Lookup("reset-deployment-id"))
```

`ConfigureRootCommand` (`src/internal/app/cli.go`) calls `viper.SetEnvPrefix("INFRAHUB")`,
`viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))`, and `viper.AutomaticEnv()`, so those
two keys would resolve `INFRAHUB_DECRYPT_KEY` and `INFRAHUB_RESET_DEPLOYMENT_ID`. But
`restoreCmd`'s `RunE` reads `restoreDecryptKey` and `restoreResetDeploymentID` — the
`BoolVar`/`StringVar` targets — which only ever hold what the command line supplied. The
`--latest` route passes the same two variables into `app.RestoreLatestBackup`, so it behaves
identically.

There is no config-file support in this CLI (nothing calls `viper.ReadInConfig`), so the
practical impact is limited to the two environment variables.

### Shape of the rest of the file

`restore` is the only command with this mismatch. For comparison:

- `createCmd` binds its flags **and** reads them back through `viper.GetBool` /
  `viper.GetString` in `RunE` — consistent, and its environment variables do work.
- `pruneCmd`'s `--dry-run` / `--force` / `--s3` are deliberately not bound at all and are read
  from locals — consistent.
- `restoreCmd`'s new `--latest` / `--s3` are deliberately not bound and are read from locals —
  consistent, and documented as per-invocation switches.
- The retention flags on `create` and `prune` are deliberately not bound, for reasons recorded
  in the comments next to them.
- `restoreCmd`'s `--sleep` is not bound; note that the viper key `sleep` is bound to
  **`create`'s** `--sleep`, so `INFRAHUB_SLEEP` affects `create` only. That is arguably
  correct but is worth confirming when this is fixed.

### Fix direction

Pick one and make it explicit:

- **Support the environment variables**: have `RunE` read `viper.GetString("decrypt-key")` and
  `viper.GetBool("reset-deployment-id")`, matching `create`'s shape. Note that
  `decrypt-key`'s value is a path to a private key; supplying it via the environment is
  reasonable for unattended restores and would make it usable from a scheduled job.
- **Drop the bindings**: delete the two `BindPFlag` calls and add a comment saying restore
  flags are command-line only, matching `prune`. Smaller change, but removes a capability the
  bindings advertise.

Either way, add a test that pins the decision, and update
`docs/docs/reference/commands.mdx` — its `restore` flag table has no environment-variable
column today, which happens to match the current (broken) behavior.
