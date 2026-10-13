# 14. Restoring into an externally-managed database needs its own authorisation, and the channel that granted it is recorded

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/spec.md (FR-009, FR-010, SC-005), contracts/cli-surface.md

## Context

A restore already has an override (`--force`) and a scheduled, unattended mode. Spec 007 adds a
destructive operation of a new kind: overwriting a database the deployment does not manage, on a
server the customer's other systems may also depend on. Neither existing switch was granted with
that in mind, and an unattended restore can reach the new path with nobody present.

## Decision

- **A distinct authorisation**: `--allow-external-restore` on the command line, or
  `INFRAHUB_ALLOW_EXTERNAL_RESTORE=true` in configuration. `--force` does not imply it, and enabling
  scheduled restore does not imply it. Making either imply it is a breaking change to the CLI
  contract.
- **The channel is part of the decision.** An unattended restore into an external database is
  possible only when the authorisation came through configuration (FR-010). The resolved value
  therefore carries its source (`flag` or `config`), not only a boolean.
- **An explicit flag wins over the environment in either direction**, so an operator can refuse on
  the command line what a scheduled job's configuration permits. A malformed environment value
  authorises nothing, and says so in the log.
- **The flag is deliberately not bound through viper.** Viper reports a flag and its environment
  equivalent through one key, and this decision turns on telling them apart; the two channels are
  read separately.
- **Refused at the gate, before anything is stopped**, with a message naming the authorisation, its
  flag and its configuration key. There is no path from "absent" to a destructive step.

## Consequences

- Nobody acquires the capability to overwrite an unmanaged database by enabling something else.
- A scheduled restore against an external database has exactly one configuration key that makes it
  possible, and its presence is auditable.
- One flag on the root command is registered without a viper binding, with a comment saying why,
  which is an exception to the repository's binding convention and looks like an omission until
  read.

## Alternatives Considered

- **Reuse `--force`** — it already means "overwrite the deployment's own data"; extending it to a
  database outside the deployment grants a capability nobody asked for when they set it.
- **Imply it from the scheduled-restore toggle** — the unattended path is where an absent operator
  makes the risk highest, not lowest.
- **A single viper-bound key** — cannot distinguish an interactive grant from a configured one,
  which FR-010 requires.
