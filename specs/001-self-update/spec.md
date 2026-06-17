# Feature Specification: Self-Update Command

**Feature Branch**: `001-self-update`  
**Created**: 2026-06-17  
**Status**: Draft  
**Input**: User description: "I want to be able to do infrahub-backup update or something to update itself. is this possible"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Update to the latest version (Priority: P1)

An operator running an older version of the tool wants to move to the latest
released version without manually finding the download location, fetching the
correct file for their operating system and architecture, and swapping it into
place. They run a single `update` command and the tool replaces itself with the
newest available release.

**Why this priority**: This is the core value of the feature — getting to the
latest version with one command. Everything else (checking, pinning, channel
selection) is refinement around this central action. Delivered alone, it
already removes the manual download-and-replace burden.

**Independent Test**: Install an older version, run `update`, confirm the binary
reports the newest released version afterward and still runs correctly.

**Acceptance Scenarios**:

1. **Given** an installed older version and network access, **When** the user runs `update`, **Then** the tool downloads the latest release for the user's platform, replaces the running binary, and reports the version it upgraded to.
2. **Given** the installed version is already the latest, **When** the user runs `update`, **Then** the tool reports it is already up to date and makes no changes.
3. **Given** the download or replacement fails partway through, **When** the failure occurs, **Then** the original working binary remains in place and the user is told the update did not complete.

---

### User Story 2 - Check for updates without installing (Priority: P2)

An operator wants to know whether a newer version exists — for change planning,
changelog review, or maintenance scheduling — without actually replacing the
binary yet.

**Why this priority**: A safe, read-only check builds trust in the update
mechanism and supports cautious operators and automation that wants to gate
upgrades. It is valuable but secondary to performing the update itself.

**Independent Test**: Run the check command on an older install and confirm it
reports that a newer version is available, including the target version, and
changes nothing on disk.

**Acceptance Scenarios**:

1. **Given** a newer release exists, **When** the user runs the update check, **Then** the tool reports the current version, the latest available version, and that an update is available, without modifying the binary.
2. **Given** the installed version is the latest, **When** the user runs the update check, **Then** the tool reports that no update is available.

---

### User Story 3 - Confirm or skip confirmation before replacing (Priority: P3)

An operator wants the update to be a deliberate action by default (showing what
will change and asking before replacing the binary), while automation wants to
run the update unattended without an interactive prompt.

**Why this priority**: Safe defaults and an automation escape hatch are quality
refinements that make the feature suitable for both hands-on and scripted use,
but they are not required for the basic update flow to deliver value.

**Acceptance Scenarios**:

1. **Given** an interactive session and an available update, **When** the user runs `update`, **Then** the tool shows the current and target versions and asks for confirmation before replacing the binary.
2. **Given** a non-interactive or scripted run, **When** the user supplies an option to skip confirmation, **Then** the update proceeds without prompting.

---

### Edge Cases

- **No network / release source unreachable**: The tool reports it could not reach the release source and leaves the current binary unchanged.
- **No release exists for the user's OS/architecture**: The tool reports that no compatible release is available and makes no changes.
- **Insufficient permissions to replace the binary** (e.g., installed in a system path owned by root): The tool reports the permission problem and instructs the user how to re-run with sufficient privileges, without leaving a partial install.
- **Tool installed via a package manager** (e.g., Homebrew): The tool detects that it is under external management and directs the user to update through that manager instead of self-replacing, to avoid breaking the package manager's tracking.
- **Tool running inside a container image**: Self-replacement of an image-baked binary is not durable across container restarts; the tool reports that the container image should be updated instead.
- **Downloaded file is corrupt or fails integrity verification**: The tool discards the download, reports the verification failure, and leaves the current binary in place.
- **Interrupted mid-replacement** (process killed, power loss): The original binary must remain runnable; an interrupted update must never leave the user with a non-working tool.
- **Sibling binary**: The repository ships two binaries (`infrahub-backup` and `infrahub-taskmanager`). Running `update` from one updates that invoked binary; the user is informed if the sibling on the same system is also out of date.
- **Windows freshly-downloaded executable**: A newly written, unsigned executable may be quarantined or flagged by Windows Defender (a known issue for similar tools, e.g. uv). The tool should complete the swap as normal; durable mitigation is code-signing the released binaries, which is out of scope for this iteration but noted as a follow-up.
- **API rate limiting / restricted network**: On shared IPs, CI, or SSH sessions the unauthenticated GitHub API rate limit (60 requests/hour) can be exhausted. The tool honors an optional access token from the environment to raise that limit; without one it reports the rate-limit condition clearly rather than failing opaquely.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The tool MUST provide an `update` command that, when run, determines the latest available released version and — if newer than the running version — downloads and installs it in place of the currently running binary.
- **FR-002**: The tool MUST detect the running binary's operating system and architecture and select the matching release artifact automatically.
- **FR-003**: The tool MUST compare the installed version against the latest available version and skip replacement (reporting "already up to date") when the installed version is current or newer.
- **FR-004**: The tool MUST verify the integrity of any downloaded artifact before using it to replace the running binary, and MUST abort the update if verification fails.
- **FR-005**: The tool MUST perform the replacement atomically with respect to the running binary, such that a failure or interruption at any point leaves the previous working binary in place and runnable.
- **FR-006**: The tool MUST report, on completion, the version it upgraded from and the version it upgraded to.
- **FR-007**: The tool MUST provide a way to check for an available update without installing it, reporting current version, latest version, and whether an update is available, while making no changes on disk.
- **FR-008**: The tool MUST, by default in an interactive session, show the current and target versions and ask for confirmation before replacing the binary, and MUST provide an option to skip the prompt for unattended/scripted use.
- **FR-009**: The tool MUST allow the user to target a specific version rather than only the latest, so a known-good version can be installed or a downgrade performed.
- **FR-010**: The tool MUST detect when it was installed by an external package manager or is running from a container image and, in those cases, decline to self-replace and instead direct the user to the appropriate update path.
- **FR-011**: The tool MUST surface clear, actionable messages for failure conditions (no network, no compatible artifact, insufficient permissions, verification failure) without leaving a partial or broken install.
- **FR-012**: The update capability MUST be available from both shipped binaries (`infrahub-backup` and `infrahub-taskmanager`), each updating the binary that was invoked.
- **FR-013**: The tool MUST treat a development/unversioned build (one not produced from a tagged release) as not eligible for self-update and report this rather than attempting to replace it.

### Key Entities *(include if feature involves data)*

- **Release**: A published version of the tool, identified by a version tag, comprising one downloadable artifact per supported operating-system/architecture combination plus integrity information for each artifact.
- **Installed Binary**: The currently running executable, characterized by its reported version, its file location on disk, its OS/architecture, and how it was installed (direct download, package manager, or container image).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user can move from an older installed version to the latest release with a single command, with no manual download or file-placement steps.
- **SC-002**: The update completes (download, verify, replace) in under 60 seconds on a typical broadband connection for the standard binary size.
- **SC-003**: 100% of failed or interrupted updates leave a working binary behind — no run results in a non-executable or partially written tool.
- **SC-004**: A user can determine whether they are on the latest version in under 10 seconds via the read-only check, without altering their installation.
- **SC-005**: When the tool cannot or should not self-update (package-managed, container, dev build, unsupported platform), the user receives a clear reason and the correct alternative path in 100% of those cases.
- **SC-006**: Support requests related to "how do I upgrade the tool" are reduced after release, with upgrades no longer requiring manual download instructions.

## Assumptions

- Updates are distributed from the project's official public release distribution point, and the latest version and per-platform artifacts can be discovered from it programmatically. (The project currently publishes tagged releases with cross-compiled binaries for Linux, Darwin, and Windows on amd64/arm64.)
- Version identifiers follow the existing release tagging scheme, allowing a reliable "is newer than" comparison between the installed and available versions.
- The user running the command has, or can obtain (e.g., via elevated privileges), write access to the binary's location; the tool's job is to detect and clearly report when they do not, not to escalate privileges itself.
- "Latest" refers to the latest stable release by default; targeting a specific version (FR-009) covers pinning and downgrade needs without introducing a separate pre-release channel in this iteration.
- Self-update applies to the standalone binary installation method. Package-manager and container deployments are explicitly out of scope for self-replacement and are handled by directing users to their native update path.
