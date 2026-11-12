# 9. PostgreSQL is reached only through `psql`, `pg_dump` and `pg_restore` inside `task-manager-db`

**Status**: Accepted
**Date**: 2026-11-12
**Source**: specs/archive/006-taskmgr-db-storage/research.md (R1) and spec Assumptions; [ADR 0001](0001-shell-out-to-docker-kubectl-clis.md); pull request [#193](https://github.com/opsmill/infrahub-backup/pull/193)

## Context

The three binaries run on an operator's host, outside the deployment's network. They reach the deployment only through the `docker` and `kubectl` command-line tools (ADR 0001), and `docker exec` and `kubectl exec` open no network path to the PostgreSQL port of `task-manager-db`. `infrahub-backup` has always run `pg_dump` and `pg_restore` inside that container. The storage figures of the task manager database (spec 006) added read queries and had to choose how to run them. Until pull request #193, `github.com/jackc/pgx/v5` was a dependency, used only to parse the Prefect connection URL.

## Decision

Every PostgreSQL command runs inside the `task-manager-db` container, which on Kubernetes is the pod that the shared pod lookup selects, the primary of a CloudNativePG cluster. It connects over TCP to `localhost` as the database owner, with the user, password and database that the Prefect connection URL holds. The host and port of that URL are ignored: the connection is always `localhost:5432` inside the container.

Queries run through `psql -h localhost -U <owner> -d <database> -X -q -A -t -v ON_ERROR_STOP=1 -c <query>`. `-X` ignores a `.psqlrc` file in the container, and `-A -t` prints the bare value without alignment or headers. Each query returns one JSON value that PostgreSQL builds with `json_build_object` and `json_agg`, and the tool parses that JSON. It never splits `psql` text output on delimiters, because flow names and event names can contain any character.

The tool has no PostgreSQL client library. The Prefect connection URL is parsed with `net/url`, so a user, password or database that the URL leaves out takes the tool's default, never a value from `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `~/.pgpass` or the operating-system user of the host.

## Consequences

- The `task-manager-db` image must ship `psql`, `pg_dump`, `pg_restore` and `sh`. The official `postgres`, Bitnami and CloudNativePG images do. Without `psql`, the collectors record `psql missing: <cause>` and the bundle is still produced.
- The tool needs only the privileges of the database owner, and no superuser right: the owner reads its own tables and the statistics views, and every session parameter the tool sets is one that any role can set (ADR 0008). The non-superuser `prefect` role that the Infrahub production deployment documentation describes, owner of the `prefect` database and of the `public` schema, works for both collectors and for `infrahub-backup create`. This was checked on 2026-11-12 against `postgres:18-alpine` with password authentication required on `localhost`.
- A PostgreSQL server outside the deployment cannot be reached. Without a `task-manager-db` service, the collectors are `skipped` with the reason `service not deployed; the task manager may use an external PostgreSQL server`.
- `go.mod` carries no database driver, so database work does not change the Nix vendor hash or the cross-compilation.
- The credentials must reach the process inside the container. `infrahub-collect` writes the password on stdin (ADR 0008); `infrahub-backup create` still passes it as `-e PGPASSWORD=<password>` or `env PGPASSWORD=<password>`.

## Alternatives Considered

- A Go PostgreSQL client, with `pgx` already a dependency: rejected, because the tool has no network path to the database port through `docker exec` or `kubectl exec`, and because ADR 0001 keeps all deployment access on the `docker` and `kubectl` command-line tools.
- Unix-socket peer authentication as the `postgres` operating-system user: rejected, because it depends on the image. The Bitnami image that the Helm chart uses and CloudNativePG do not accept it for the database owner.
- Running the queries in the Prefect server container, `task-manager`: rejected, because a large listing there once got the server killed for running out of memory, and the server is the single point of failure of the whole task pipeline.
