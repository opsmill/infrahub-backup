# 15. Connections to external databases are encrypted and verified by default, with one operator opt-out

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/spec.md (FR-029), contracts/cli-surface.md (T100 change of record), research.md (R4)

## Context

The external path opens connections this toolset never opened before: Bolt from the transient pod
to the Neo4j servers for the version probe and the seed statement, and libpq to PostgreSQL for the
dump and the load. Both carry the tool's own database credentials out of the cluster. The
deployment records TLS settings for Infrahub's own connections (`INFRAHUB_DB_TLS_ENABLED`,
`_TLS_INSECURE`, `_TLS_CA_FILE`), but a deployment left at Infrahub's defaults exports none of
them — so "absent" is the common observation, and the first implementation, which encrypted only
when the deployment said `true`, sent the password in clear by default with no flag able to
prevent it.

## Decision

- **Encrypted and verified is the default for both databases.** The deployment's settings decide
  *how* to verify, never *whether*.
- **`--external-db-insecure-tls` is the only opt-out** (FR-029). With it, the channel is the weakest
  one the deployment's own settings describe: encrypted but unverified where the deployment declares
  TLS, unencrypted where it declares none. An external Neo4j with no TLS at all is therefore still
  reachable, but reaching it takes the operator saying so.
- **A discovered insecure setting is reported and never obeyed.** `INFRAHUB_DB_TLS_INSECURE=true`
  is a decision the deployment made about Infrahub's connections, not about this tool's.
- **The deployment's own certificate authority is carried into the workload** that has to verify
  against it, read from the running component that records it (`INFRAHUB_DB_TLS_CA_FILE`, or the
  Prefect URL's `sslrootcert`) while that component is still up, and placed once per workload: a
  keytool import for the JVM clients, `PGSSLROOTCERT` for libpq. A CA that cannot be read or placed
  falls back to the image's default trust store — the weakest *verifying* configuration, never a
  non-verifying one, so the worst outcome is an "unknown issuer" the operator can act on.
- **The decision is two-valued, so there is no policy-name flag.** `cypher-shell`'s verification is
  chosen by the Bolt URI scheme and libpq's by `PGSSLMODE`, and in both the tool's choice is
  verify or the operator's opt-out. `--external-db-tls-policy` was registered and removed for that
  reason, and stays removed.

## Consequences

- A deployment that exports no TLS settings gets an encrypted, verified channel, and a server
  presenting a privately signed certificate needs its CA recorded in the deployment or the
  operator's opt-out.
- The CA read has an ordering dependency: it happens at the gate, before the deployment's
  components are scaled down for an offline capture. Re-reading it later fails and degrades to
  the system trust store.
- The TLS decision is resolved once per database per run and passed to every command builder, so
  no builder decides it for itself.

## Alternatives Considered

- **Follow the deployment's `tls_enabled`** — true that a server not speaking TLS cannot be made to,
  but the setting is usually absent, and absent sent the password in clear.
- **A `--external-db-tls-policy` string** — would have had nothing to carry beyond what the boolean
  does.
- **Obey a discovered `tls_insecure`** — a setting made about something else, silently disabling
  verification for credentials this tool sends.
- **Fail when the CA cannot be placed** — turns a degraded-but-verifying connection into no backup
  at all, for a condition the operator can see in the warning.
