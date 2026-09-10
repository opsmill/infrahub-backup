<!-- Extracted from specs/007-external-database-backup on 2026-09-07 -->

# Implementation Verification Guidelines

## Overview

Two checks that a chunk of implementation must pass before it is reported done, whoever or
whatever wrote it. Both come from the spec 007 retrospective: eight implementation chunks each
reported full success with accurate, honest unit-test evidence, and a later review found five
requirements that were completely inert — about 470 lines with no non-test caller — plus a pod the
kubelet rejected unconditionally, built on a premise about the official images that ten seconds of
`docker image inspect` would have falsified.

Unit tests written alongside the code prove that it compiles and that the author's assertions are
self-consistent. They cannot tell working code from unreachable code, correct logic wired to the
wrong thing, or a guarantee that holds per chunk and fails in composition.

## Grep for callers of every new symbol, per chunk

Before a chunk is reported complete, every newly added exported or package-level function, type
and variable must have at least one caller outside `_test.go` files, or a written reason why not.

```sh
grep -rn 'newSymbol(' src/ | grep -v _test
```

Zero hits means the requirement the symbol implements is not wired in, whatever the tests say. Do
not defer this to a final review: the cost of finding it per chunk is one grep, the cost of finding
it at review was a partial reset.

## Verify premises about external artifacts with a named command

Any factual claim about something outside this repository — an image's user, a chart's labels, a
server's exit codes, a metric's presence on a version — is verified by running a command and
recording it, not asserted from memory or documentation. The retrospective's example:

```console
$ docker image inspect postgres:18-alpine --format '{{.Config.User}} {{.Config.Entrypoint}}'
 [docker-entrypoint.sh]
```

The empty first field is the finding: the image declares no user.

Record the command and its output where the premise is used (a code comment, the chunk report,
or a `dev/knowledge/` page when the fact is durable, as
[container-image-user-semantics.md](../knowledge/container-image-user-semantics.md) does).

## What a passing test suite does prove

It proves the change compiles, that its own assertions hold, and that it did not break an
assertion someone else wrote. It is necessary. It is not evidence of reachability, of composition
across chunks, or of any fact about the world outside the repository — which is what the two
checks above are for.
