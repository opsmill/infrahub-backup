# Infrahub Ops CLI Changelog

This is the changelog for `infrahub-backup`, `infrahub-taskmanager` and `infrahub-collect`.
All notable changes to this project will be documented in this file.

Issue tracking is located in [GitHub](https://github.com/opsmill/infrahub-backup/issues).

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This project uses [*towncrier*](https://towncrier.readthedocs.io/) and the changes for the upcoming release can be found in <https://github.com/opsmill/infrahub-backup/tree/main/changelog/>.

Releases up to and including v2.3.0 predate this file; their notes are on the [GitHub releases page](https://github.com/opsmill/infrahub-backup/releases).

<!-- towncrier release notes start -->

## [2.5.0](https://github.com/opsmill/infrahub-backup/tree/v2.5.0) - 2026-11-13

### Added

- Added `--include-traces` to `infrahub-collect create`. It adds the traces of slow requests from Tempo to the support bundle, slowest first, with personal data removed, within a size limit, and records the trace sampling ratio when an Infrahub API token is given. On Docker Compose the collector reads the published port 3200 of the `infrahub-tempo` service; on Kubernetes it forwards a port to the service labelled `app.kubernetes.io/name=tempo`, which needs `list` and `get` on `services`, `get` and `list` on `pods`, and `create` on `pods/portforward`; none of these is needed with `--traces-tempo-url`. Both are new assumptions about the deployment. The collector works with Tempo 2.5 and later, including the Tempo of the `infrahub-observability` Helm chart. The `bundle_information.json` version changed to `2026110500`: collector entries can now have a `details` object.

## [2.4.2](https://github.com/opsmill/infrahub-backup/tree/v2.4.2) - 2026-11-10

### Fixed

- `infrahub-backup restore` into a Neo4j cluster now waits for the servers to finish dropping the database before it restores the backup. Before, the restore sometimes failed with `Database with name [neo4j] has replicated cluster state locally`, most often with a large database. `DROP DATABASE` returned as soon as the cluster had recorded the drop, while the server where the restore runs could still be stopping the database, and `neo4j-admin database restore` refuses to run until that server has removed its cluster state for the database. The restore now sends `DROP DATABASE` with `WAIT`. If the server still holds that state when `WAIT` runs out, the restore tries `neo4j-admin database restore` again every 5 seconds for up to 5 minutes. After that, it fails with an error that says to run the restore again once the server has finished dropping the database.

## [2.4.1](https://github.com/opsmill/infrahub-backup/tree/v2.4.1) - 2026-11-05

### Fixed

- `infrahub-backup restore` into a Neo4j cluster now creates the restored database from the server that holds the restored data. The tool read the ID of that server through a routed `cypher-shell` connection, which ran the query on the server that leads the `system` database and so returned that server's ID. When the leader was another server, it had no data to copy from, every server reported the database `offline` with `No store found on any of the seeders`, and the restore failed.
- `infrahub-backup restore` into a Neo4j cluster now fails when the restored database does not come online on every server that hosts it. Before it fails, it logs one line for each server where the database is not online, with the status and the reason Neo4j gives, and the error lists the same information. While it waits, it logs the status of each server whenever it changes, as a warning once a server reports a reason. Before, the restore stopped waiting after 100 status checks, 2 seconds apart, and reported success even when no server had the database online. The next step then failed instead: with `--reset-deployment-id`, the deployment ID reset failed with `Unable to get a routing table for database 'neo4j' because this database is unavailable`; without it, Infrahub restarted against a database that was not available. The restore now waits up to 10 minutes by default, so that the other servers can copy a large store from the seed server; `--neo4j-cluster-online-timeout` changes this wait.
- `infrahub-backup restore` into a Neo4j cluster whose default query language is Cypher 25, such as a new Neo4j 2026.05 installation, now creates the restored database with the `existingDataSeedServer` option when Neo4j refuses `existingDataSeedInstance`. Before, the restore dropped the database and then failed to create it again, with `Invalid input 'existingdataseedinstance' for 'OPTIONS'`. Neo4j servers that accept `existingDataSeedInstance` still receive it.
- `infrahub-backup restore` now empties the cache by deleting the keys of every Redis database with `redis-cli`, instead of deleting the files under `/data` while Redis runs. On Docker Compose, Redis keeps its data in memory and writes it back to disk when it stops, so the keys stored before the restore were still there after the restore. The keys are deleted by a Lua script, because the Bitnami Redis chart that the Infrahub Helm chart uses disables `FLUSHALL` and `FLUSHDB` by default. With Redis Sentinel, the restore deletes the keys on the master, and the replicas follow. When the Redis container sets `REDIS_PASSWORD` or `REDIS_PASSWORD_FILE`, as the Bitnami image does, the restore uses that password.
- `infrahub-backup restore` now empties the message queue by deleting its RabbitMQ queues with `rabbitmqctl`, instead of deleting the files under `/var/lib/rabbitmq` while RabbitMQ runs. On Docker Compose, RabbitMQ could restart after that file deletion with a durable queue it could not start, and the Infrahub server then stopped during its startup with `NOT_FOUND - home node ... of durable queue 'infrahub.rpcs' in vhost '/' is down or inaccessible`. On Kubernetes, the file deletion removed nothing, because `/var/lib/rabbitmq` is a symbolic link in the Bitnami RabbitMQ image, so a RabbitMQ with persistent storage kept the messages queued before the restore; the restore now deletes them there too. The restore keeps the broker's users, permissions and policies, and does not change the membership of a RabbitMQ cluster.

## [2.4.0](https://github.com/opsmill/infrahub-backup/tree/v2.4.0) - 2026-10-26

### Changed

- A backup now stops with an error, before any service is stopped, when the Neo4j edition cannot be determined. Before, the tool assumed Community and stopped the Infrahub services for an offline dump.

### Fixed

- `restore` no longer reports an Enterprise backup as incompatible with a Community Edition deployment when it could not detect the target's Neo4j edition. A failed edition query now stops the restore of an Enterprise backup with an error that gives the reason the query failed, names the `infrahub-server` container when the Neo4j credentials could not be read from it, and says how to supply them. Community Edition backups are still restored when the edition cannot be detected, and Plakar restores of a snapshot without Neo4j data no longer depend on the edition query. ([#168](https://github.com/opsmill/infrahub-backup/issues/168))
- On Kubernetes, `infrahub-backup create` with Neo4j Community and `infrahub-backup restore` now read the replica count of each workload they scale before they change anything in the deployment, and stop with an error that includes kubectl's message when a count cannot be read. Previously, a workload whose count could not be read was scaled down and came back with 1 replica, with no message at the default log level. A warning that kubectl wrote while it read the count was enough to cause this. `infrahub-collect create --include-backup` runs the same backup and has the same fix. A restore also no longer scales the task manager database (`task-manager-db`) to 1 replica when it restores the task manager data: it keeps the replica count the workload had. ([#182](https://github.com/opsmill/infrahub-backup/issues/182))
- Backups on Kubernetes no longer send the Neo4j edition query to a PostgreSQL pod when the task manager database runs on an operator that labels its pods `app.kubernetes.io/component=database`, such as CloudNativePG. The tool now finds each service's pods by the `infrahub/service` label first, and finds Neo4j by the Neo4j Helm chart's pod label before it tries the generic labels. Before this, the backup could assume Neo4j Community and stop the Infrahub services.
- On Docker Compose deployments that run more than one `task-worker` replica, such as Infrahub's default of two, `infrahub-collect create` could record the `telemetry` collector as `failed` and leave the telemetry export out of the bundle. The export was written in one `task-worker` container and then copied from another. The export, the copy, and the cleanup now run in the same container.

### Housekeeping

- Manage the changelog with towncrier news fragments, matching the setup used across the other OpsMill repositories.
- Pull requests that change behaviour now fail unless they carry a news fragment, so the changelog is written while the author still has the context instead of being reconstructed when the release is cut.
- Releases are prepared as reviewable pull requests before a merge publishes binaries, images, and the Homebrew formula.
- The Kubernetes end-to-end tests deploy Infrahub with `INFRAHUB_DB_LIVENESS_CHECK_TIMEOUT=0`, so Infrahub replaces a closed Neo4j connection instead of answering `503 Service Unavailable`, and they wait until Infrahub can query Neo4j before they create or check data. A Community test that started right after the previous test replaced the Neo4j pod could fail with a 503.
- The end-to-end tests serve their S3 bucket from a single-node Garage container (`dxflrs/garage`) instead of MinIO, and the release workflow pulls the MinIO client image from `registry.opsmill.io/external/mc` instead of Docker Hub. The MinIO server image the tests used is no longer available from Docker Hub.
