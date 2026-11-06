# Contract: Backup artifact and metadata

**Feature**: 007-external-database-backup

The artifact is a cross-version contract. The constitution requires that any change to snapshot
layout or metadata preserve the ability to restore backups created by prior released versions, or
document a migration path. This feature takes the first option: **additive only, no migration**.

---

## Layout: unchanged

The artifact layout produced for an external database is byte-compatible with the one produced for
an internal database of the same edition. This is the point of the design — the vendor tooling runs
in a container either way, and only the container's origin differs.

Two consequences worth stating, because a previous branch in this repository hit both:

- The entry MUST be named from the **target database**, not a literal default name. A restore reads
  `<targetdb>`-derived names from the source path, so an artifact named for the wrong database
  restores only when the names happen to coincide.
- The **raw vendor artifact** is carried, not an archive of a directory containing it. An online
  Neo4j Enterprise backup produces exactly one artifact file, so there is nothing to archive, and
  an archive is not a valid input to restore.

An artifact taken from an external database MUST therefore be restorable to an internal deployment
and vice versa. This is a test obligation, not just a statement (see quickstart).

## Metadata: additive fields

| Field | Type | Absent means |
|---|---|---|
| `source_endpoints` | ordered list, per database | Endpoints not recorded — a pre-feature artifact |
| `source_roles` | map endpoint → role, per database | Roles not observed |
| `capture_complete` | bool | Complete — a pre-feature artifact was never partial by this definition |
| `external_location` | bool, per database | Internal |

`source_roles` carries `unknown` as a real value, distinct from the field being absent, and the
contract deliberately stops short of naming which endpoint served the capture. With several
endpoints supplied there is no documented interface that reports it, so recording one would assert
provenance the tool never established — the class of quiet false confidence Principle II targets.
What the operator gets instead is enough to see that a follower *could* have served the capture,
which is the fact that actually bears on the recovery point.

## Reader rules

A reader encountering an artifact with **none** of these fields MUST treat it as an internal,
complete capture from an unrecorded endpoint — which is precisely what every previously released
version produced. No operator action, no migration, no warning.

A reader should never encounter `capture_complete: false`, because FR-012 requires an incomplete
capture to be removed rather than retained. The field is defence in depth: if removal itself fails,
the artifact is still identifiable as unusable. A reader that does encounter it MUST NOT offer it as
a restore point.

This is what makes the version story safe. Retention decides from artifact names and recency ranks
and never reads metadata, so had incomplete artifacts been retained they would have occupied a
keep slot and evicted a good backup — and an older tool, which ignores unknown fields, would have
restored one without noticing. Removing them instead resolves both without touching retention and
without bumping the metadata version (FR-024).

## Writer rules

- `capture_complete` is derived from what the backup operation **reported**, not from its exit
  status. A backup that reached only some of several endpoints exits with the same status as one
  that failed outright, so exit status alone cannot establish completeness (research R6).
- `source_endpoints` records what was supplied, in try order. It is not a claim that the first
  entry served the capture, and no field claims that of any entry.
- Metadata is written for internal captures too, so the fields do not themselves signal
  externality — `external_location` does.

## Restore-side staging (added by T048/T049, and this is the change of record)

Restoring into an **external** Neo4j inverts the data path: the server fetches the artifact
itself, so the artifact has to be somewhere it can read from. The tool therefore writes one object
the backup path never wrote — the raw `<database>-<timestamp>.backup` extracted from the archive,
staged in the configured bucket under one extra key segment:

```text
s3://<bucket>/<prefix>/seed/<database>-<timestamp>.backup
```

Four properties of that key are load-bearing:

- **It is the existing `--s3-*` surface, not a new flag.** The artefact store a restore stages into
  is the bucket the operator already configures; a run with none configured is refused before any
  destructive step, naming those flags (FR-007, FR-020).
- **The extra segment keeps it out of retention.** Retention recognises an object only where its key
  is exactly the one `buildS3Key` would produce for a backup's own base name, so an object one
  segment deeper is invisible to it — no keep slot is occupied and no good backup is evicted.
- **It is removed only once the restored database reports online.** Until then the server may still
  be fetching it. A failed restore deliberately leaves it in place, with its URI in the error, so it
  can be examined.
- **Only `s3`, `gs` and `azb` are accepted.** Those are the seed providers a Neo4j server carries as
  shipped; every other scheme needs `dbms.databases.seed_from_uri_providers` configured on each
  server, which the tool cannot observe and must not depend on (research R2).

An internal restore stages nothing and writes no object: this section applies only where the gate
established that the database lives outside the deployment (FR-015).

## Restore-side compatibility

Restore version tolerance is forward-only: an artifact restores into the same or a later database
version, not an earlier one. The existing edition-resolution rules are unchanged — a Community
artifact restores by the Community path, and an Enterprise artifact still cannot be restored onto
a Community server.

## Change control

Adding a field is additive and requires only that absence keeps its documented meaning. Changing
the meaning of an existing field, renaming one, or altering the layout is a breaking change and
requires a documented migration path plus a release-note callout, per the constitution's
backup-engine and deployment-contract clauses.
