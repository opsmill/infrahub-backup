# Contract: CLI surface

**Feature**: 007-external-database-backup

The public interface of a CLI tool is its flags, its environment variables, its exit behaviour and
the wording of its failures. All four are contracts here — the failure wording especially, because
SC-008 requires an operator to identify the remedy from the message alone.

Flags follow the existing convention: registered on the root command, bound through viper, and
overridable by `INFRAHUB_`-prefixed environment variables with `-` replaced by `_`.

---

## New flags

| Flag | Applies to | Default | Purpose |
|---|---|---|---|
| `--neo4j-address` | backup, restore | *(discovered)* | Override the Neo4j host list. Accepts `host[:port][,host[:port]...]`, the same shape Infrahub's own address setting accepts. |
| `--neo4j-backup-port` | backup | `6362` | The backup service port. Not discoverable; see research R4. |
| `--postgres-address` | backup, restore | *(discovered)* | Override the PostgreSQL endpoint as `host[:port]`. |
| `--external-db-image-neo4j` | backup, restore | *(pinned default)* | Image supplying the Neo4j administration tooling. Flag only — not environment-settable (FR-027). |
| `--external-db-image-postgres` | backup, restore | *(pinned default)* | Image supplying the PostgreSQL client tooling. Flag only — not environment-settable (FR-027). |
| `--external-db-scratch-size` | backup | *(derived from database size)* | Scratch space for the transient workload (FR-022). |
| `--external-db-insecure-tls` | backup, restore | `false` | Do not protect connections to an external database server: skip certificate verification, and accept an unencrypted channel where the deployment records TLS as disabled. The **only** opt-out from FR-029; neither a discovered `INFRAHUB_DB_TLS_INSECURE` nor an absent `INFRAHUB_DB_TLS_ENABLED` implies it. |
| `--allow-external-restore` | restore | `false` | The authorisation required by FR-009. Distinct from `--force`. |
| `--external-db-timeout` | backup, restore | *(generous default)* | Bound on any single operation against an external database (FR-025). |
| `--external-db-image-pull-secrets` | backup, restore | *(none)* | Comma-separated secrets in the deployment's namespace holding registry credentials for the tooling image. |
| `--external-db-service-account` | backup, restore | *(none)* | Service account the transient workload runs under. It still mounts no service-account token. |
| `--external-db-node-selector` | backup, restore | *(none)* | Comma-separated `key=value` node labels the transient workload must be scheduled onto. |
| `--external-db-tolerations` | backup, restore | *(none)* | Comma-separated taints the workload tolerates, written as a taint is: `key=value:Effect`, `key:Effect`, or a bare `key`. |
| `--external-db-priority-class` | backup, restore | *(none)* | PriorityClass the transient workload is admitted under. |

**T111 added the five placement flags above, and this is the change of record.** The manifest
declared no `imagePullSecrets`, `serviceAccountName`, `nodeSelector`, `tolerations` or
`priorityClassName`, and nothing could supply them — which made the image flags unusable for the
case they exist for. An operator who must mirror the tooling image into their own registry has a
registry that needs credentials, so the workload sat in `ImagePullBackOff` until the readiness
bound expired, and the only diagnosis available named a condition the operator had no input to
fix. The same held for a namespace whose capable nodes are tainted, or which admits a pod only
under a named service account or priority class.

Each field is absent from the manifest unless its flag is supplied, so a deployment that needs
none of this gets exactly the pod it got before (FR-015). A malformed value is refused before any
object is created, by a message naming the flag — the same reason `--external-db-scratch-size` is
parsed by this tool rather than by the API server.

Unlike the image flags, these follow the binding convention and are environment-settable. FR-027's
reason for keeping the image out of ambient environment does not reach them: the danger there is
that the tool holds workload-creation rights, so an environment-settable image turns those rights
into a means of running arbitrary code in the customer's namespace. These say where a pod may be
placed and which credentials fetch its image, never what it runs, and the image itself stays
pinned or flag-only. A named service account grants the pod no API access either —
`automountServiceAccountToken` stays `false`, so what an account carries here is its registry
credentials and whatever the namespace's admission requires. An unattended backup is configured by
environment at least as often as by argv, and a scheduled run in a mirrored-registry cluster with
no way to name its pull secret is the defect restated rather than fixed.

`--external-db-tls-policy` was registered by T001 and removed by T095, which recorded that T041
would reintroduce it alongside the channel it configures. T041 landed that channel and **did not**
reintroduce it. The channel takes no policy name: `cypher-shell`'s verification is selected by the
Bolt URI scheme (`+s` verifies, `+ssc` accepts a self-signed certificate) and `psql`/`pg_dump`'s by
`PGSSLMODE` (`verify-full` or `require`), and in both the decision this tool has to make is
two-valued — verify, or the operator's explicit opt-out. `--external-db-insecure-tls` above carries
exactly that, and a string naming a policy would have had nothing left to carry. The removal
therefore stands; anything finer-grained is a change to this contract rather than a restoration.

**T100 widened what the opt-out covers, and this is the change of record.** The Neo4j channel was
encrypted only when the deployment exported `INFRAHUB_DB_TLS_ENABLED=true` — but a deployment that
leaves that setting at Infrahub's own default exports nothing, so the common observation is
*absent*, and absent sent this tool's own database password out of the cluster in clear with no
flag able to prevent it. Encryption is now the default for both databases, and
`--external-db-insecure-tls` is the one input that turns it off: with the flag, the channel is the
weakest one the deployment's own settings describe — unverified where the deployment declares TLS,
unencrypted where it does not. So an external Neo4j with no TLS at all is still reachable, but
reaching it takes the operator saying so. The decision stays two-valued, so no new flag is
introduced and `--external-db-tls-policy` stays removed.

**Interaction rules**:

- `--force` MUST NOT imply `--allow-external-restore`, and enabling scheduled restore MUST NOT
  imply it either (FR-009). Any change making one imply the other is a breaking change to this
  contract.
- Unattended restore MUST read the authorisation from configuration, not from an interactive
  prompt (FR-010). The flag form exists for interactive use.
- Address overrides apply per database. Overriding one MUST NOT change resolution of the other
  (FR-001).

## Configuration keys

Every flag above has an `INFRAHUB_`-prefixed environment equivalent, per the existing binding
convention — **except the image flags**, which stay the sole exception (the placement flags added
by T111 are bound; see the reasoning above). Those resolve from a pinned default or an explicitly
supplied flag only, never from ambient environment (FR-027): the tool holds workload-creation
rights, so an environment-settable image would turn those rights into a means of running arbitrary
images in the customer's namespace.

The authorisation key is the one that matters for unattended operation:

```text
INFRAHUB_ALLOW_EXTERNAL_RESTORE=true
```

Its presence in a scheduled restore's configuration is the only way an unattended destructive
restore into an externally-managed database can occur.

## Consumed environment (read from in-deployment components, not from the operator)

| Variable | Used for |
|---|---|
| `INFRAHUB_DB_ADDRESS` | Host list — comma-separated `host[:port]` |
| `INFRAHUB_DB_PORT` | Client (Bolt) port; **not** the backup port |
| `INFRAHUB_DB_PROTOCOL` | `bolt` or `neo4j` |
| `INFRAHUB_DB_DATABASE`, `_USERNAME`, `_PASSWORD` | Already read today |
| `INFRAHUB_DB_TLS_ENABLED`, `_TLS_INSECURE`, `_TLS_CA_FILE` | Client connection TLS |
| Prefect connection URL | PostgreSQL host, port, database, user, password |

Reading these is additive: a deployment that does not set them behaves exactly as today.

## Exit behaviour

| Condition | Exit | Requirement |
|---|---|---|
| Backup complete, all endpoints reached | 0 | |
| Backup produced an artifact but some endpoints were unreachable | non-zero, partial artifact removed from every location it reached | FR-012 |
| Deployment query failed (permission, unreachable control plane) | non-zero, **never** treated as external | FR-002 |
| Client tooling older than the server | non-zero, before any data is read | FR-006 |
| Unsupported edition for external capture | non-zero, naming the mechanism limitation | FR-008 |
| External restore attempted without authorisation | non-zero, database untouched | FR-009 |
| Restore artifact unreachable by the server | non-zero, database untouched | FR-007 |
| Restore accepted but database did not reach a usable state | non-zero | FR-021 |
| Restore accepted and the database's state can no longer be read | non-zero well before the operation bound, attributed to the path to the database rather than to the database | FR-021, FR-025 |
| An external database stops responding mid-operation | non-zero at the bound, Infrahub returned to prior scale | FR-025 |
| Restricted pod-security namespace rejects the workload | non-zero, naming the security context requirement | FR-027 |

The tool MUST NOT surface a database vendor's exit code directly where that code conflates
outcomes — notably a backup exit status that means either failure or partial success (research R6).

## Failure message contract

Each message names the condition, the specific resource, and the operator action. Wording is
illustrative; the three required elements are not.

| Condition | Must name |
|---|---|
| No container and no discoverable endpoint | that discovery was attempted, where it looked, and the override flag |
| No container and the deployment's own settings could not be read | that the settings were *not* read and so what they record is unknown, the cluster's own reason, and both remedies (the override flag, or restoring access) — explicitly *not* that no setting records an address (FR-019, distinct from the row above) |
| Deployment query denied | that the query failed and which permission is missing — explicitly *not* that the database is absent |
| Missing workload-creation permission | the verbs and resource required |
| External Community edition | that the Community mechanism requires direct access to the database's own storage, and is therefore unavailable remotely — not "service not found" |
| Version incompatibility | both versions, and which one must change |
| Missing authorisation for external restore | the authorisation, its flag and its configuration key |
| Server cannot read the artifact | the URI attempted and the server-side prerequisite |
| Restore did not come online | the database's reported status, and where the server logs the cause |
| The restored database's state cannot be read at all | that the failure is in the path to the database (the transient pod, the cluster API, the exec permission) and not a verdict on the database, which may still be loading — explicitly *not* the row above's "the database is not usable", and not the server's own logs |
| `infrahub-collect --include-backup` against an external database | that the capture needs a transient pod and secret in the namespace, that collection creates no workload (ADR-0003), and that `infrahub-backup create` performs it |

## Backwards compatibility

For a deployment whose databases are all internal, the entire surface above is inert: no new flag
is required, no new environment variable is consulted to a different effect, and behaviour and
artifact layout are unchanged (FR-015). New flags are additive and default to the current
behaviour. No documented deployment service name is added or renamed (FR-017) — the constitution
treats those names as a public contract, so the transient workload registers under an existing one.
