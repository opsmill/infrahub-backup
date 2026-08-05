package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// neo4jScriptedBackend answers the queries the Neo4j backup and restore paths make against
// a container, and records every command, copy, and their order.
//
// The order is the point: the defect these tests pin was entirely a matter of which command
// ran before which — a dump that outlived its backup, and a copy into a directory that had
// not been cleared — so a fake that only recorded *that* the commands ran could not tell the
// fixed path from the broken one.
type neo4jScriptedBackend struct {
	bareBackend

	// calls is every step in the order it happened. Commands are recorded joined by
	// spaces; copies are recorded as "copy-to <src> <dest>" / "copy-from <src> <dest>".
	calls []string

	// failures maps a command prefix to the error its execution reports, which is how a
	// container that refuses one specific step is simulated.
	failures map[string]error

	// dumpBody is written by CopyFrom, so the archive the backup path assembles holds a
	// real file to checksum and tar.
	dumpBody string
	// copyToErr fails the restore's copy into the container.
	copyToErr error
}

func newNeo4jScriptedBackend() *neo4jScriptedBackend {
	return &neo4jScriptedBackend{
		bareBackend: bareBackend{name: "docker"},
		failures:    map[string]error{},
		dumpBody:    "neo4j dump",
	}
}

func (b *neo4jScriptedBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	joined := strings.Join(command, " ")
	b.calls = append(b.calls, joined)

	for prefix, err := range b.failures {
		if strings.HasPrefix(joined, prefix) {
			return "", err
		}
	}

	switch {
	case command[0] == "cat" && command[1] == neo4jPIDFile:
		return "42\n", nil
	case command[0] == "uname":
		// The architecture the embedded watchdog is selected for.
		return "x86_64\n", nil
	case command[0] == "whoami":
		return "neo4j\n", nil
	case strings.Contains(joined, "/proc/42/status"):
		// waitForProcessStopped reads the process state; T is "stopped", which is what
		// lets stopNeo4jCommunity return on its first poll.
		return "T (stopped)\n", nil
	}

	return "", nil
}

func (b *neo4jScriptedBackend) CopyTo(service, src, dest string) error {
	b.calls = append(b.calls, "copy-to "+src+" "+dest)

	return b.copyToErr
}

func (b *neo4jScriptedBackend) CopyFrom(service, src, dest string) error {
	b.calls = append(b.calls, "copy-from "+src+" "+dest)

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return os.WriteFile(dest, []byte(b.dumpBody), 0o600)
}

var _ EnvironmentBackend = (*neo4jScriptedBackend)(nil)

// newNeo4jTestOps is an InfrahubOps whose environment is already resolved to the scripted
// backend, so no detection runs and every step goes to the fake.
func newNeo4jTestOps(backend EnvironmentBackend) *InfrahubOps {
	iops := NewInfrahubOps()
	iops.backend = backend
	iops.config.Neo4jDatabase = "neo4j"

	return iops
}

// indexOf is the position of the first recorded call with the given prefix, or -1.
func indexOf(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, prefix) })
}

// TestBackupNeo4jCommunityRemovesTheRemoteDump pins the create half of the wrong-dump
// defect: the dump `create` writes inside the container is a complete, loadable archive of
// the deployment, and it lands in the directory a later restore copies into. If it survives
// the backup, a restore can load it instead of the archive it was given — and report
// success while doing so.
func TestBackupNeo4jCommunityRemovesTheRemoteDump(t *testing.T) {
	remoteDump := neo4jRemoteWorkDir + "/neo4j.dump"

	tests := []struct {
		name string
		// failures are the container steps that fail, so the cleanup is judged on the
		// paths where the dump exists but the backup does not finish.
		failures map[string]error
		wantErr  bool
	}{
		{
			name: "after a successful backup",
		},
		{
			name:     "after a dump that could not be copied out",
			failures: map[string]error{"neo4j-admin database dump": errors.New("dump interrupted")},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := newNeo4jScriptedBackend()
			for prefix, err := range tc.failures {
				backend.failures[prefix] = err
			}
			iops := newNeo4jTestOps(backend)

			err := iops.backupNeo4jCommunity(t.TempDir())

			if tc.wantErr && err == nil {
				t.Fatalf("backupNeo4jCommunity() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("backupNeo4jCommunity() = %v, want nil", err)
			}

			removal := indexOf(backend.calls, "rm -f "+remoteDump)
			if removal < 0 {
				t.Fatalf("the remote dump %s was never removed; calls: %v", remoteDump, backend.calls)
			}

			// Removed while the container is still reachable — the resume that ends the
			// backup is the last thing that happens, so a cleanup after it would be a
			// cleanup nothing guarantees can still run.
			if resume := indexOf(backend.calls, "kill -CONT"); resume >= 0 && removal > resume {
				t.Errorf("the dump was removed after neo4j was resumed; calls: %v", backend.calls)
			}
		})
	}
}

// TestBackupNeo4jCommunityLeavesNoLoadableDumpForARestore is the same property stated the
// way the defect was observed: after a `create`, nothing that a restore's
// `--from-path=/tmp/infrahubops` would read is left in the container.
func TestBackupNeo4jCommunityLeavesNoLoadableDumpForARestore(t *testing.T) {
	backend := newNeo4jScriptedBackend()
	iops := newNeo4jTestOps(backend)

	if err := iops.backupNeo4jCommunity(t.TempDir()); err != nil {
		t.Fatalf("backupNeo4jCommunity() = %v, want nil", err)
	}

	// The dump written by --to-path, and the only file in that directory the loader would
	// pick up, is gone by the time the backup returns.
	dumped := indexOf(backend.calls, "neo4j-admin database dump")
	removed := indexOf(backend.calls, "rm -f "+neo4jRemoteWorkDir+"/neo4j.dump")
	if dumped < 0 {
		t.Fatalf("no dump was taken; calls: %v", backend.calls)
	}
	if removed < 0 {
		t.Fatalf("the dump was left in %s for a later restore to load; calls: %v", neo4jRemoteWorkDir, backend.calls)
	}
	if removed < dumped {
		t.Errorf("the dump was removed before it was taken; calls: %v", backend.calls)
	}
}

// TestRestoreNeo4jClearsTheDestinationBeforeCopying pins the restore half of the same
// defect. Neither backend replaces an existing destination directory — `docker cp` nests
// the source inside it, `kubectl cp` merges into it — so a copy into a directory that was
// not cleared puts the archive's dump somewhere the loader may not read, and lets whatever
// was already there be loaded in its place.
func TestRestoreNeo4jClearsTheDestinationBeforeCopying(t *testing.T) {
	workDir := t.TempDir()
	backupPath := filepath.Join(workDir, "backup", "database")

	backend := newNeo4jScriptedBackend()
	iops := newNeo4jTestOps(backend)

	if err := iops.restoreNeo4j(workDir, neo4jEditionEnterprise, false); err != nil {
		t.Fatalf("restoreNeo4j() = %v, want nil", err)
	}

	clear := indexOf(backend.calls, "rm -rf "+neo4jTempBackupDir)
	copy := indexOf(backend.calls, "copy-to "+backupPath+" "+neo4jTempBackupDir)

	if copy < 0 {
		t.Fatalf("the archive's dump was never copied into the container; calls: %v", backend.calls)
	}
	if clear < 0 {
		t.Fatalf("%s was never cleared; calls: %v", neo4jTempBackupDir, backend.calls)
	}
	if clear > copy {
		t.Errorf("%s was cleared only after the copy, so a leftover still decides what is loaded; calls: %v",
			neo4jTempBackupDir, backend.calls)
	}
}

// TestRestoreNeo4jFailsWhenTheDestinationCannotBeCleared covers the reason that clearing is
// not best-effort: a destination that could not be emptied is a destination that may still
// hold another backup's dump, and continuing would restore data the operator did not ask
// for while reporting success. Failing before the copy is what keeps that from happening.
func TestRestoreNeo4jFailsWhenTheDestinationCannotBeCleared(t *testing.T) {
	clearFailure := errors.New("exit status 128")

	backend := newNeo4jScriptedBackend()
	backend.failures["rm -rf "+neo4jTempBackupDir] = clearFailure
	iops := newNeo4jTestOps(backend)

	err := iops.restoreNeo4j(t.TempDir(), neo4jEditionEnterprise, false)

	if err == nil {
		t.Fatalf("restoreNeo4j() = nil, want an error")
	}
	if !errors.Is(err, clearFailure) {
		t.Errorf("error = %q, want it to wrap %v", err, clearFailure)
	}
	if !strings.Contains(err.Error(), neo4jTempBackupDir) {
		t.Errorf("error = %q, want it to name %s", err, neo4jTempBackupDir)
	}
	if indexOf(backend.calls, "copy-to ") >= 0 {
		t.Errorf("the archive was copied into a directory that could not be cleared; calls: %v", backend.calls)
	}
	if indexOf(backend.calls, "neo4j-admin database restore") >= 0 {
		t.Errorf("a restore ran despite the destination not being cleared; calls: %v", backend.calls)
	}
}
