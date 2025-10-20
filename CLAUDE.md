# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`infrahub-ops-cli` is a Go-based toolset for managing and maintaining Infrahub instances. The project provides two specialized CLI binaries:

- **infrahub-backup** - Backup/restore operations and environment detection
- **infrahub-taskmanager** - Task manager (Prefect) maintenance operations

Both tools share common internal application logic but expose different commands through their respective main entry points.

## Common Development Commands

### Building and Running

- `make build` - Build both binaries to `bin/infrahub-backup` and `bin/infrahub-taskmanager`
- `make build-all` - Cross-compile both binaries for Linux, Darwin, and Windows (amd64/arm64)
- `make install` - Build and install both binaries to `$GOPATH/bin`
- `make clean` - Remove build artifacts

### Testing and Quality

- `make test` - Run all tests
- `make test-coverage` - Generate coverage report (outputs coverage.html)
- `make lint` - Run golangci-lint (note: errcheck is disabled in .golangci.yaml)
- `make fmt` - Format code with go fmt
- `make vet` - Run go vet

### Development Setup

- `make dev-setup` - Install development dependencies including golangci-lint
- `make deps` - Download and tidy dependencies
- `make deps-update` - Update all dependencies

## Architecture

The codebase follows a command-pattern architecture using Cobra for CLI structure, with two separate binary entry points sharing common internal logic:

### Core Components

1. **src/cmd/infrahub-backup/main.go** - Backup tool entry point
   - Defines root command with backup/restore subcommands
   - Commands: `create`, `restore`, `environment detect`, `environment list`, `version`
   - Uses shared application logic from `src/internal/app`

2. **src/cmd/infrahub-taskmanager/main.go** - Task manager tool entry point
   - Defines root command with task management subcommands
   - Commands: `flush flow-runs`, `flush stale-runs`, `environment detect`, `environment list`, `version`
   - Uses shared application logic from `src/internal/app`

3. **src/internal/app/app.go** - Core application logic
   - `InfrahubOps` struct - Main application controller
   - `CommandExecutor` - Handles Docker Compose and system command execution
   - Environment detection (Docker vs Kubernetes)
   - Docker project discovery and validation
   - Shared by both CLI tools

4. **src/internal/app/backup.go** - Backup and restore operations
   - Creates tar.gz backups with metadata JSON
   - Backs up Neo4j database, PostgreSQL (task-manager), and artifacts
   - Implements safe backup with container stopping/starting
   - Restore validates metadata and handles version compatibility

5. **src/internal/app/taskmanager.go** - Task management operations
   - PostgreSQL database connection management
   - Flow run cleanup operations (completed/failed/cancelled)
   - Stale run cancellation (stuck in running state)
   - Uses embedded Python scripts for Prefect API operations

6. **src/internal/app/utils.go** - Utility functions
   - File operations, checksum validation
   - Environment variable handling
   - Version detection and comparison

7. **src/internal/app/cli.go** - Shared CLI configuration
   - `ConfigureRootCommand()` - Sets up common flags and configuration
   - `AttachEnvironmentCommands()` - Adds environment detection commands
   - Shared between both binaries

### Key Design Patterns

- **Split Binary Architecture**: Two specialized binaries sharing common internal logic for focused functionality
- **Embedded Scripts**: Python scripts are embedded using Go's embed package (src/internal/app/scripts directory)
- **Docker Compose Integration**: All operations work through Docker Compose commands
- **Project-based Operations**: Can target specific Docker Compose projects with `--project` flag
- **Streaming Output**: Commands stream output in real-time for user feedback
- **Shared Configuration**: Both binaries use the same configuration system and environment variables

## Docker Compose Dependencies

Both tools assume Infrahub is deployed using Docker Compose with these service names:

- `database` (Neo4j) - Used by infrahub-backup
- `task-manager-db` (PostgreSQL) - Used by both tools
- `infrahub-server`, `task-worker`, `task-manager`, `task-manager-background-svc` - Application containers
- `cache`, `message-queue` - Infrastructure services

## Error Handling

The codebase uses explicit error wrapping with `fmt.Errorf` for context. All commands return errors up to the Cobra command handlers which handle display to users.

**Documentation Purpose:**

- Guide users through installing, configuring, and using Infrahub in real-world workflows
- Explain concepts and system architecture clearly, including new paradigms introduced by Infrahub
- Support troubleshooting and advanced use cases with actionable, well-organized content
- Enable adoption by offering approachable examples and hands-on guides that lower the learning curve

**Structure:** Follows [Diataxis framework](https://diataxis.fr/)

- **Tutorials** (learning-oriented)
- **How-to guides** (task-oriented)
- **Explanation** (understanding-oriented)
- **Reference** (information-oriented)

**Tone and Style:**

- Professional but approachable: Avoid jargon unless well defined. Use plain language with technical precision
- Concise and direct: Prefer short, active sentences. Reduce fluff
- Informative over promotional: Focus on explaining how and why, not on marketing
- Consistent and structured: Follow a predictable pattern across sections and documents

**For Guides:**

- Use conditional imperatives: "If you want X, do Y. To achieve W, do Z."
- Focus on practical tasks and problems, not the tools themselves
- Address the user directly using imperative verbs: "Configure...", "Create...", "Deploy..."
- Maintain focus on the specific goal without digressing into explanations
- Use clear titles that state exactly what the guide shows how to accomplish

**For Topics:**

- Use a more discursive, reflective tone that invites understanding
- Include context, background, and rationale behind design decisions
- Make connections between concepts and to users' existing knowledge
- Present alternative perspectives and approaches where appropriate
- Use illustrative analogies and examples to deepen understanding

**Terminology and Naming:**

- Always define new terms when first used. Use callouts or glossary links if possible
- Prefer domain-relevant language that reflects the user's perspective (e.g., playbooks, branches, schemas, commits)
- Be consistent: follow naming conventions established by Infrahub's data model and UI

**Reference Files:**

- Documentation guidelines: `docs/docs/development/docs.mdx`
- Vale styles: `.vale/styles/`
- Markdown linting: `.markdownlint.yaml`

### Document Structure Patterns (Following Diataxis)

**How-to Guides Structure (Task-oriented, practical steps):**

```markdown
- Title and Metadata
    - Title should clearly state what problem is being solved (YAML frontmatter)
    - Begin with "How to..." to signal the guide's purpose
    - Optional: Imports for components (e.g., Tabs, TabItem, CodeBlock, VideoPlayer)
- Introduction
    - Brief statement of the specific problem or goal this guide addresses
    - Context or real-world use case that frames the guide
    - Clearly indicate what the user will achieve by following this guide
    - Optional: Links to related topics or more detailed documentation
- Prerequisites / Assumptions
    - What the user should have or know before starting
    - Environment setup or requirements
    - What prior knowledge is assumed
- Step-by-Step Instructions
    - Step 1: [Action/Goal]
        - Clear, actionable instructions focused on the task
        - Code snippets (YAML, GraphQL, shell commands, etc.)
        - Screenshots or images for visual guidance
        - Tabs for alternative methods (e.g., Web UI, GraphQL, Shell/cURL)
        - Notes, tips, or warnings as callouts
    - Step 2: [Action/Goal]
        - Repeat structure as above for each step
    - Step N: [Action/Goal]
        - Continue as needed
- Validation / Verification
    - How to check that the solution worked as expected
    - Example outputs or screenshots
    - Potential failure points and how to address them
- Advanced Usage / Variations
    - Optional: Alternative approaches for different circumstances
    - Optional: How to adapt the solution for related problems
    - Optional: Ways to extend or optimize the solution
- Related Resources
    - Links to related guides, reference materials, or explanation topics
    - Optional: Embedded videos or labs for further learning
```

**Topics Structure (Understanding-oriented, theoretical knowledge):**

```markdown
- Title and Metadata
    - Title should clearly indicate the topic being explained (YAML frontmatter)
    - Consider using "About..." or "Understanding..." in the title
    - Optional: Imports for components (e.g., Tabs, TabItem, CodeBlock, VideoPlayer)
- Introduction
    - Brief overview of what this explanation covers
    - Why this topic matters in the context of Infrahub
    - Questions this explanation will answer
- Main Content Sections
    - Concepts & Definitions
        - Clear explanations of key terms and concepts
        - How these concepts fit into the broader system
    - Background & Context
        - Historical context or evolution of the concept/feature
        - Design decisions and rationale behind implementations
        - Technical constraints or considerations
    - Architecture & Design (if applicable)
        - Diagrams, images, or explanations of structure
        - How components interact or relate to each other
    - Mental Models
        - Analogies and comparisons to help understanding
        - Different ways to think about the topic
    - Connection to Other Concepts
        - How this topic relates to other parts of Infrahub
        - Integration points and relationships
    - Alternative Approaches
        - Different perspectives or methodologies
        - Pros and cons of different approaches
- Further Reading
    - Links to related topics, guides, or reference materials
    - External resources for deeper understanding
```

### Quality and Clarity Checklist

**General Documentation:**

- Content is accurate and reflects the latest version of Infrahub
- Instructions are clear, with step-by-step guidance where needed
- Markdown formatting is correct and compliant with Infrahub's style
- Spelling and grammar are checked with Vale
- **Vale style checks pass**: Run `vale $(find ./docs -type f \( -name "*.mdx" -o -name "*.md" \) -not -path "./docs/node_modules/*")` and address all issues

**For Guides:**

- The guide addresses a specific, practical problem or task
- The title clearly indicates what will be accomplished
- Steps follow a logical sequence that maintains flow
- Each step focuses on actions, not explanations
- The guide omits unnecessary details that don't serve the goal
- Validation steps help users confirm their success
- The guide addresses real-world complexity rather than oversimplified scenarios

**For Topics:**

- The explanation is bounded to a specific topic area
- Content provides genuine understanding, not just facts
- Background and context are included to deepen understanding
- Connections are made to related concepts and the bigger picture
- Different perspectives or approaches are acknowledged where relevant
- The content remains focused on explanation without drifting into tutorial or reference material
- The explanation answers "why" questions, not just "what" or "how"
