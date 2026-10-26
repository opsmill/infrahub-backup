# Infrahub Ops CLI Changelog

This is the changelog for `infrahub-backup`, `infrahub-taskmanager` and `infrahub-collect`.
All notable changes to this project will be documented in this file.

Issue tracking is located in [GitHub](https://github.com/opsmill/infrahub-backup/issues).

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This project uses [*towncrier*](https://towncrier.readthedocs.io/) and the changes for the upcoming release can be found in <https://github.com/opsmill/infrahub-backup/tree/main/changelog/>.

Releases up to and including v2.3.0 predate this file; their notes are on the [GitHub releases page](https://github.com/opsmill/infrahub-backup/releases).

<!-- towncrier release notes start -->

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
