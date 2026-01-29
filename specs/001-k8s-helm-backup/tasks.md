# Tasks: Kubernetes Helm Chart Support for Infrahub Backup

**Input**: Design documents from `/specs/001-k8s-helm-backup/`
**Prerequisites**: plan.md (required), spec.md (required)

**Important**: This implementation spans **two repositories**:
- `opsmill/infrahub-ops-cli` (this repo): Dockerfile, Makefile updates, GitHub Actions
- `opsmill/infrahub-helm` (external): Helm subchart + integration into main chart

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1, US2, US3, US4)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Docker Image - infrahub-ops-cli repo)

**Purpose**: Create container image for Kubernetes deployment

- [x] T001 Create multi-arch Dockerfile with Go builder and alpine runtime in Dockerfile
- [x] T002 Update Makefile with docker-build, docker-build-multi, and docker-push targets in Makefile
- [x] T003 Add Docker build and push job to release workflow in .github/workflows/release.yml

**Checkpoint**: Docker image can be built and pushed for linux/amd64 and linux/arm64

---

## Phase 2: Foundational (Helm Chart Base - infrahub-helm repo)

**Purpose**: Core Helm chart structure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T004 Create Chart.yaml with metadata and version in charts/infrahub-backup/Chart.yaml
- [x] T005 [P] Create values.yaml with complete default configuration in charts/infrahub-backup/values.yaml
- [x] T006 [P] Create _helpers.tpl with name, fullname, labels, selectorLabels, serviceAccountName helpers in charts/infrahub-backup/templates/_helpers.tpl
- [x] T007 [P] Create ServiceAccount template with conditional creation in charts/infrahub-backup/templates/serviceaccount.yaml
- [x] T008 [P] Create Role template with pod exec and deployment/statefulset permissions in charts/infrahub-backup/templates/role.yaml
- [x] T009 [P] Create RoleBinding template linking ServiceAccount to Role in charts/infrahub-backup/templates/rolebinding.yaml

**Checkpoint**: Foundation ready - Helm chart structure validated with `helm lint charts/infrahub-backup`

---

## Phase 3: User Story 1 - Scheduled Backup to S3 (Priority: P1) 🎯 MVP

**Goal**: Enable automated periodic backups via CronJob that store artifacts in S3-compatible storage

**Independent Test**: Deploy chart with `backup.enabled=true`, `backup.mode=cronjob`, and S3 configuration. Verify backup artifacts appear in S3 bucket on schedule.

### Implementation for User Story 1

- [x] T010 [US1] Create CronJob template with S3 environment variables in charts/infrahub-backup/templates/cronjob-backup.yaml
- [x] T011 [US1] Add schedule, concurrencyPolicy, historyLimit configuration to CronJob in charts/infrahub-backup/templates/cronjob-backup.yaml
- [x] T012 [US1] Configure S3 credentials from Secret reference in CronJob template in charts/infrahub-backup/templates/cronjob-backup.yaml
- [x] T013 [US1] Add resource limits, nodeSelector, tolerations, affinity to CronJob spec in charts/infrahub-backup/templates/cronjob-backup.yaml

**Checkpoint**: User Story 1 complete - CronJob backup to S3 works independently

---

## Phase 4: User Story 2 - One-Shot Backup to S3 (Priority: P2)

**Goal**: Enable on-demand backup via single Job deployment for maintenance windows

**Independent Test**: Deploy chart with `backup.enabled=true`, `backup.mode=job`, and S3 configuration. Verify single backup artifact appears in S3.

### Implementation for User Story 2

- [x] T014 [US2] Create Job template for one-shot backup with S3 support in charts/infrahub-backup/templates/job-backup.yaml
- [x] T015 [US2] Add conditional rendering based on backup.mode (job vs cronjob) in charts/infrahub-backup/templates/job-backup.yaml
- [x] T016 [US2] Configure Job completion and failure handling (restartPolicy: Never) in charts/infrahub-backup/templates/job-backup.yaml

**Checkpoint**: User Story 2 complete - One-shot backup Job to S3 works independently

---

## Phase 5: User Story 3 - Restore from S3 (Priority: P2)

**Goal**: Enable restore from S3 backup artifact via Job deployment

**Independent Test**: Deploy chart with `restore.enabled=true` and S3 artifact location. Verify Infrahub data matches backup state.

### Implementation for User Story 3

- [x] T017 [US3] Create restore Job template with S3 download support in charts/infrahub-backup/templates/job-restore.yaml
- [x] T018 [US3] Configure S3 artifact path (bucket/key) as command argument in charts/infrahub-backup/templates/job-restore.yaml
- [x] T019 [US3] Add restore-specific options (excludeTaskmanager, migrateFormat) in charts/infrahub-backup/templates/job-restore.yaml
- [x] T020 [US3] Configure S3 credentials from Secret reference for restore in charts/infrahub-backup/templates/job-restore.yaml

**Checkpoint**: User Story 3 complete - Restore from S3 works independently

---

## Phase 6: User Story 4 - Local Backup Storage (Priority: P3)

**Goal**: Enable local storage of backup artifacts when S3 is not available

**Independent Test**: Deploy chart with `backup.storage.type=local`. Run backup and verify artifact exists in pod/volume at configured path.

### Implementation for User Story 4

- [x] T021 [US4] Add local storage path configuration to backup Job/CronJob templates in charts/infrahub-backup/templates/cronjob-backup.yaml
- [x] T022 [US4] Add local storage path configuration to backup Job template in charts/infrahub-backup/templates/job-backup.yaml
- [x] T023 [US4] Add conditional volume mount for PersistentVolume support in charts/infrahub-backup/templates/cronjob-backup.yaml
- [x] T024 [US4] Add conditional volume mount to backup Job template in charts/infrahub-backup/templates/job-backup.yaml

**Checkpoint**: User Story 4 complete - Local backup storage works independently

---

## Phase 7: Polish & Integration

**Purpose**: Integrate subchart into main Infrahub chart and final validation

- [x] T025 Add infrahub-backup as conditional dependency in charts/infrahub/Chart.yaml (snippet created in helm-charts/infrahub-integration-snippets/)
- [x] T026 Add infrahub-backup default values section in charts/infrahub/values.yaml (snippet created in helm-charts/infrahub-integration-snippets/)
- [ ] T027 Run helm dependency update for infrahub chart in charts/infrahub/ (deferred - run in infrahub-helm repo)
- [x] T028 Validate chart with helm lint for all templates in charts/infrahub-backup/
- [x] T029 Validate template rendering with helm template for backup scenarios
- [x] T030 Validate template rendering with helm template for restore scenarios

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately (this repo)
- **Foundational (Phase 2)**: Can start in parallel with Phase 1 (different repo) - BLOCKS all user stories
- **User Stories (Phase 3-6)**: All depend on Foundational phase completion
  - User stories can proceed sequentially in priority order (P1 → P2 → P3)
- **Polish (Phase 7)**: Depends on all user stories being complete

### User Story Dependencies

- **User Story 1 (P1)**: Can start after Foundational (Phase 2) - No dependencies on other stories
- **User Story 2 (P2)**: Can start after Foundational (Phase 2) - Shares template patterns with US1
- **User Story 3 (P2)**: Can start after Foundational (Phase 2) - Independent restore Job
- **User Story 4 (P3)**: Depends on US1 and US2 templates being created (adds local storage support)

### Within Each User Story

- Templates require RBAC (from Foundational) to be in place
- S3 configuration patterns shared across backup and restore
- Local storage extends existing templates

### Parallel Opportunities

- Phase 1 (this repo) and Phase 2 (infrahub-helm repo) can run in parallel
- All Foundational tasks T005-T009 marked [P] can run in parallel
- US1, US2, US3 can start in parallel once Foundational is complete
- US4 should wait for US1/US2 templates to be created first

---

## Parallel Example: Foundational Phase

```bash
# Launch all Foundational tasks together:
Task: "Create values.yaml with complete default configuration in charts/infrahub-backup/values.yaml"
Task: "Create _helpers.tpl with helpers in charts/infrahub-backup/templates/_helpers.tpl"
Task: "Create ServiceAccount template in charts/infrahub-backup/templates/serviceaccount.yaml"
Task: "Create Role template in charts/infrahub-backup/templates/role.yaml"
Task: "Create RoleBinding template in charts/infrahub-backup/templates/rolebinding.yaml"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (Docker image)
2. Complete Phase 2: Foundational (Helm chart base)
3. Complete Phase 3: User Story 1 (CronJob backup to S3)
4. **STOP and VALIDATE**: Test scheduled backup independently
5. Deploy/demo if ready - this delivers primary disaster recovery capability

### Incremental Delivery

1. Complete Setup + Foundational → Foundation ready
2. Add User Story 1 → Test scheduled backup → Deploy (MVP!)
3. Add User Story 2 → Test one-shot backup → Deploy (adds maintenance window support)
4. Add User Story 3 → Test restore → Deploy (completes disaster recovery)
5. Add User Story 4 → Test local storage → Deploy (adds fallback option)
6. Add Integration → Test with main chart → Deploy (seamless Helm experience)

### Cross-Repository Coordination

This feature requires changes in two repositories:

1. **First**: Complete Phase 1 in `opsmill/infrahub-ops-cli` and merge/release
2. **Then**: Complete Phases 2-7 in `opsmill/infrahub-helm` using the published Docker image
3. **Integration**: Subchart uses `opsmill/infrahub-backup:latest` (or tagged version)

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story should be independently completable and testable
- Commit after each task or logical group
- Stop at any checkpoint to validate story independently
- Dockerfile must include kubectl for Kubernetes operations
- S3 credentials must come from Kubernetes Secrets (never plain values)
- Prerequisites: PR #56 (S3 support) must be merged before this implementation
