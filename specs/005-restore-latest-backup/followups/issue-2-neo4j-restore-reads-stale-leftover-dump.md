# Follow-up issue draft 2

Ready to file on `opsmill/infrahub-backup`. Not yet filed — awaiting approval.

Suggested labels: `bug`, `data-integrity`, `priority/high`

## Title

Neo4j restore loads a leftover dump from a previous `create` instead of the requested archive

## Body

### Summary

On the tarball backend with Neo4j Community, `create` leaves its dump behind at
`/tmp/infrahubops/<database>.dump` inside the `database` container. A later `restore` copies
the requested archive's dump into that same directory, where it lands one level deeper, and
then loads `--from-path=/tmp/infrahubops` — which resolves to the **leftover** dump, not the
one from the archive that was asked for.

Every `restore` performed after a `create` on the same host is affected, independent of how
the archive was chosen (`--latest`, `--s3`, or a named file). Pre-existing; predates
`restore --latest` (spec `005-restore-latest-backup`).

Severity: a restore can silently load different data than the archive it names, and report
success.

### Mechanism (confirmed by reading the code)

1. `neo4jTempBackupDir` (`src/internal/app/backup_neo4j.go:16`) and `neo4jRemoteWorkDir`
   (`src/internal/app/backup_neo4j_watchdog.go:14`) are both `"/tmp/infrahubops"` — the dump
   path and the restore path share one directory.
2. `backupNeo4jCommunity` (`src/internal/app/backup_neo4j.go:288`) creates that directory,
   dumps into it with `--to-path=/tmp/infrahubops` (`backup_neo4j.go:313-326`), copies the
   dump out with `CopyFrom` (`backup_neo4j.go:333`), and **returns without removing the
   remote dump**. Its deferred cleanup removes only the watchdog artifacts
   (`backup_neo4j.go:301-303`). Contrast `backupNeo4jEnterprise`
   (`backup_neo4j.go:205-215`), which does `rm -rf` the directory in a defer.
3. `restoreNeo4j` (`src/internal/app/backup_neo4j.go:341`) then runs
   `CopyTo("database", <workDir>/backup/database, "/tmp/infrahubops")`
   (`backup_neo4j.go:344`). Because the destination directory already exists, the source
   directory is copied *into* it, so the archive's dump lands at
   `/tmp/infrahubops/database/<database>.dump`.
4. `restoreNeo4jCommunity` (`src/internal/app/backup_neo4j.go:527`) runs
   `neo4j-admin database load --overwrite-destination=true --from-path=/tmp/infrahubops`
   (`backup_neo4j.go:558`). That path holds the leftover top-level dump from step 2; the
   archive's dump is one level down and is never read.

`restoreNeo4jCommunity` and `restoreNeo4j` both `rm -rf /tmp/infrahubops` on the way out, so
the stale dump only exists between a `create` and the next `restore` — which is precisely
the sequence an operator or a scheduled job performs.

### Observed symptoms

Two runs against a live Docker Compose deployment, both bad, differing in what state the
leftover was in:

**(a) Silent wrong-data restore.** `create` A with a tag present → delete the tag → `create`
B → `restore` A. Exit 0, `Neo4j dump restored successfully` in the output, and the tag is
**absent** afterwards: the data loaded was B's, not A's.

**(b) Hard failure.** Two real `create` runs, then `restore --latest` exits 1 with:

```text
Failed to load database 'neo4j': Not a valid Neo4j archive: /tmp/infrahubops/neo4j.dump
```

The error naming the *top-level* path is itself direct evidence of the mechanism: the
archive's dump is at `/tmp/infrahubops/database/neo4j.dump`, and the loader never looked
there.

Which symptom appears depends on the leftover's state (a complete dump from the previous
`create` versus a partially-written or otherwise unloadable one), so neither should be read
as "the" behavior — the shared defect is that the loaded file is not the requested one.

### Why existing e2e coverage does not catch it

`tests/e2e/test_docker_tarball.py` and `tests/e2e/test_docker_encryption.py` perform exactly
one real `create` before restoring, so the leftover dump *is* the archive being restored and
the wrong-file substitution is unobservable. The `--latest` local-pool test
(`test_restore_latest_from_local_pool`) fabricates its older pool entry by name for this
reason, and says so in its docstring.

The plakar backend is unaffected: it streams the dump through
`neo4j-admin database dump --to-stdout` on create (`backup_neo4j.go:68`) and
`--from-stdin` on restore (`restoreNeo4jCommunityStream`, `backup_neo4j.go:581`), so no
shared directory is involved.

### Fix direction

Either half closes it; doing both is cheapest to reason about:

1. **Clean up after the dump.** Give `backupNeo4jCommunity` a deferred
   `rm -rf /tmp/infrahubops` for the dump it created, as `backupNeo4jEnterprise` already
   does. `create` should not leave a loadable artifact in a directory the restore path reads.
2. **Do not restore into a directory that may already hold something.** Have `restoreNeo4j`
   `rm -rf` the destination and recreate it empty before `CopyTo`, so the copy's shape is
   deterministic — or point `--from-path` at the nested directory the copy actually produced.

A regression test needs **two real `create` runs** before the restore, and must assert on the
restored *data* (the tag from the first backup), not only on the exit code — symptom (a)
exits 0.
