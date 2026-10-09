package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Three requirements had no task naming them and so no test asserting them.
// They are grouped here because what they have in common is that each is a
// promise about a failure — an artefact that cannot be reached, a runtime that
// is not installed, an artefact written by a version that did not know about a
// field — and a promise about a failure is the kind that goes unnoticed when it
// stops being kept.

// TestRestoreRefusesAnUnreachableArtefactBeforeAnyDestructiveStep is FR-020.
//
// The requirement is an ordering, not a message: where the artefact store
// cannot be reached, the run must refuse *before* any destructive step, and
// name what it could not reach. Getting that ordering wrong is not a worse
// error message — it is a deployment scaled to zero, its transient data wiped,
// and then a restore that never had an artefact to restore from.
func TestRestoreRefusesAnUnreachableArtefactBeforeAnyDestructiveStep(t *testing.T) {
	tests := []struct {
		name    string
		storage BackendType
		restore func(iops *InfrahubOps) error
	}{
		{
			name:    "a named archive that is not there",
			storage: BackendTarball,
			restore: func(iops *InfrahubOps) error {
				missing := filepath.Join(iops.config.BackupDir, "infrahub_backup_20260101_000000.tar.gz")

				return iops.RestoreBackup(missing, false, false, 0, "", true, false)
			},
		},
		{
			// --latest against a directory holding nothing: the same absence,
			// reached through the entry point that chooses the artefact itself.
			name:    "no artefact at all under --latest",
			storage: BackendTarball,
			restore: func(iops *InfrahubOps) error {
				return iops.RestoreLatestBackup(false, false, false, 0, "", true, false)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newGatedBackend()
			iops := newGatedOps(t, backend, tt.storage)

			err := tt.restore(iops)
			if err == nil {
				t.Fatal("the restore succeeded with no artefact to restore from, want a refusal")
			}

			// Naming the prerequisite: the operator has to be able to tell an
			// unreachable artefact from a deployment that would not cooperate.
			if !strings.Contains(strings.ToLower(err.Error()), "backup") {
				t.Errorf("err = %v, want it to name the artefact it could not reach", err)
			}

			// The half that matters. Nothing may have been stopped, started or
			// run against the deployment — FR-020's "before any destructive
			// step", and constitution Principle II.
			if len(backend.stopped) > 0 {
				t.Errorf("services stopped = %v, want none: the restore took the deployment down before discovering it had nothing to restore", backend.stopped)
			}
			if len(backend.started) > 0 {
				t.Errorf("services started = %v, want none", backend.started)
			}
			for _, call := range backend.execs {
				t.Errorf("command %q ran against the deployment before the artefact was established", call)
			}
		})
	}
}

// TestEnvironmentDetectionNamesTheMissingRuntime is FR-018, and constitution
// Principle III's other half: the binaries are copied onto hosts where the
// runtime they shell out to may simply not be installed, and they must say so
// rather than report the deployment as absent.
//
// "no Infrahub environment detected" is a claim about the customer's cluster.
// When every backend was skipped because its own CLI is missing, nothing ever
// asked the cluster anything, and that message sends the operator to look in
// the wrong place entirely.
//
// It drives the real backends against an empty PATH rather than a substitute
// for them, because the behaviour under test is what happens when the tool is
// genuinely not installed — and a double that reports "not installed" would be
// asserting the test's own belief about how absence is detected.
func TestEnvironmentDetectionNamesTheMissingRuntime(t *testing.T) {
	t.Run("neither runtime installed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		iops := &InfrahubOps{config: &Configuration{}, executor: NewCommandExecutor()}

		_, err := iops.ensureBackend()
		if err == nil {
			t.Fatal("ensureBackend() = nil error with no runtime on PATH, want a refusal")
		}

		for _, want := range []string{"docker", "kubectl", "PATH"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %q — what is missing and where it was expected", err, want)
			}
		}
	})

	t.Run("a runtime that answered is not reported as missing", func(t *testing.T) {
		// kubectl present and working, but no Infrahub anywhere. That is a
		// statement about the cluster, and it must not be dressed up as a
		// missing tool — the operator would go and install what they already
		// have.
		bin := t.TempDir()
		writeStubCommand(t, filepath.Join(bin, "kubectl"))
		t.Setenv("PATH", bin)

		iops := &InfrahubOps{config: &Configuration{}, executor: NewCommandExecutor()}

		_, err := iops.ensureBackend()
		if err == nil {
			t.Fatal("ensureBackend() = nil error, want a refusal")
		}
		if strings.Contains(err.Error(), "PATH") {
			t.Errorf("err = %v, want no claim about a missing tool: kubectl was there and answered", err)
		}
	})
}

// writeStubCommand writes an executable that succeeds and prints nothing, which
// for `kubectl version --client` is a working client and for the namespace
// listing is an empty result.
func writeStubCommand(t *testing.T, path string) {
	t.Helper()

	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the stub command = %v, want nil", err)
	}
}

// TestMetadataCompatibilityHoldsInBothDirections is FR-016 and FR-024.
//
// The guarantee is that an operator never has to do anything for an artefact
// written by a different version of the tool to be restorable, and it has two
// directions. The new-reads-old direction is the one this repository exercises
// by accident every time an older artefact is restored. The old-reads-new
// direction is the one nothing exercises, because the tool that would read it
// is the one that shipped last quarter — so it is pinned here, against a
// declared copy of that older reader's shape (releasedBackupMetadata), which
// knows none of the provenance fields this feature added.
func TestMetadataCompatibilityHoldsInBothDirections(t *testing.T) {
	t.Run("the declared metadata version does not move", func(t *testing.T) {
		// FR-024 states it directly: additions MUST NOT change the artefact's
		// declared metadata version. A previously released tool that compared
		// this value would refuse an artefact it can in fact read, so a change
		// here is a compatibility break rather than a version bump.
		if metadataVersion != 2025111200 {
			t.Errorf("metadataVersion = %d, want it unchanged: moving it breaks the artefacts released tools can read (FR-024)", metadataVersion)
		}
	})

	t.Run("an old tool reads a new artefact without refusing it", func(t *testing.T) {
		var metadata releasedBackupMetadata
		if err := json.Unmarshal(newProvenanceArtefact(), &metadata); err != nil {
			t.Fatalf("an older reader refused a newer artefact: %v", err)
		}

		// Not refusing is half of it. Acting correctly on what it *can* see is
		// the other half, and the more easily lost: a reader that silently got
		// the component list wrong would restore the wrong set of databases.
		if metadata.MetadataVersion != metadataVersion {
			t.Errorf("MetadataVersion = %d, want %d", metadata.MetadataVersion, metadataVersion)
		}
		if metadata.BackupID != "20260101_120000" {
			t.Errorf("BackupID = %q", metadata.BackupID)
		}
		if metadata.Neo4jEdition != "enterprise" {
			t.Errorf("Neo4jEdition = %q, want enterprise: the restore's edition decision reads this", metadata.Neo4jEdition)
		}
		if got := metadata.Components; len(got) != 2 || got[0] != "database" || got[1] != serviceTaskManagerDB {
			t.Errorf("Components = %v, want both databases: this is what decides which are restored", got)
		}
		if metadata.Checksums["neo4j.dump"] != "abc123" {
			t.Errorf("Checksums = %v, want the artefact's own", metadata.Checksums)
		}
	})

	t.Run("a new tool reads an old artefact that states none of the optional fields", func(t *testing.T) {
		// FR-016: an artefact from a previously released version must remain
		// restorable without operator action. The fields added since are all
		// omitempty, so an older artefact simply does not carry them, and each
		// must read as the conservative value rather than as a reason to stop.
		oldArtefact := []byte(`{
			"metadata_version": 2025111200,
			"backup_id": "20250101_120000",
			"created_at": "2025-01-01T12:00:00Z",
			"tool_version": "1.0.0",
			"infrahub_version": "1.0.0",
			"components": ["database"]
		}`)

		var metadata BackupMetadata
		if err := json.Unmarshal(oldArtefact, &metadata); err != nil {
			t.Fatalf("the current reader refused an older artefact: %v", err)
		}

		if metadata.Neo4jEdition != "" {
			t.Errorf("Neo4jEdition = %q, want it unstated", metadata.Neo4jEdition)
		}
		if metadata.Encrypted || metadata.Redacted {
			t.Errorf("Encrypted/Redacted = %t/%t, want both false: an absent field is not an assertion", metadata.Encrypted, metadata.Redacted)
		}
		if metadata.Checksums != nil {
			t.Errorf("Checksums = %v, want nil rather than an empty map that reads as 'verified nothing'", metadata.Checksums)
		}

		// An unstated edition is not a refusal: ResolveRestoreEdition falls back
		// conservatively, which is what keeps such an artefact restorable
		// without operator action.
		edition, err := NewNeo4jEditionInfo("", errors.New("probe failed")).ResolveRestoreEdition(metadata.Neo4jEdition)
		if err != nil {
			t.Fatalf("an older artefact's unstated edition stopped the restore: %v", err)
		}
		if edition == "" {
			t.Error("ResolveRestoreEdition() = \"\", want a conservative fallback")
		}
	})
}
