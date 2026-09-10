<!-- Extracted from specs/007-external-database-backup on 2026-09-02 -->

# Container image user semantics

## Overview

Several planned features run vendor database tooling inside a container this tool creates, rather
than inside a container the deployment already owns. Getting the pod's security context right
depends on a fact about the official database images that is easy to assume and wrong: **they do
not declare a non-root user.**

This page records the measured behaviour, because a spec-007 implementation chunk asserted the
opposite in a code comment, shipped a pod the kubelet rejects unconditionally, and its unit tests
passed — they asserted manifest *shape*, which no unit test can connect to an admission decision.

## The measurement

```console
$ docker image inspect postgres:18-alpine --format '{{.Config.User}} {{.Config.Entrypoint}}'
 [docker-entrypoint.sh]

$ docker image inspect neo4j:2025.10.1-enterprise --format '{{.Config.User}} {{.Config.Entrypoint}}'
 [tini -g -- /startup/docker-entrypoint.sh]
```

`Config.User` is **empty** for both. An empty user means uid 0. Both images run as root and drop
privileges *inside their own entrypoint* — `postgres` via `gosu`, `neo4j` via
`/startup/docker-entrypoint.sh`.

The uids often quoted for these images (70 for the alpine `postgres` image, 7474 for `neo4j`) are
real users that exist inside the filesystem. They are not declared in the image config, so
Kubernetes does not see them.

## Why it matters

Two consequences, and the second is the one that bites:

1. `runAsNonRoot: true` with **no** `runAsUser` makes the kubelet resolve the effective user from
   the image config, find root, and refuse the container with
   `CreateContainerConfigError: container has runAsNonRoot and image will run as root`. There is no
   partial success and no fallback — every pod using that combination fails to start.
2. Setting `Command` (or `Args` alone in some shapes) **replaces the image ENTRYPOINT**, so the
   privilege drop those images rely on never runs. A pod that keeps the entrypoint and one that
   overrides it have different effective users for the same image.

A manifest that overrides the entrypoint *and* asserts `runAsNonRoot` without `runAsUser` combines
both, which is how spec 007 produced a workload that could never start.

## What to do instead

- If the pod must satisfy a restricted pod-security policy, set `runAsUser` **explicitly**. Do not
  rely on the image to declare it.
- Decide what happens for an **operator-supplied** image, whose user may differ from the pinned
  default's. Either require the operator to supply the uid alongside the image, or verify the
  image's config before building the manifest. Silently reusing the default's uid produces a pod
  that starts and then cannot read its own volume.
- Check the premise rather than inheriting it. `docker image inspect ... --format
  '{{.Config.User}}'` takes seconds and is the only authority.

## Verification

This is a claim about third-party images, so it expires. Re-measure when a pinned image tag
changes, and prefer an end-to-end case that actually admits the pod on a namespace enforcing
restricted pod security over any unit assertion about manifest fields.
