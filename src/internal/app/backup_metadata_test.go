package app

import (
	"errors"
	"strings"
	"testing"
)

// editionProbeFailureBackend is a deployment whose database service does not
// answer the Neo4j edition query, as when the query lands on a pod that is not
// Neo4j. It records every service it is asked to stop.
type editionProbeFailureBackend struct {
	bareBackend
	stopped []string
}

func (b *editionProbeFailureBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if len(command) > 0 && command[0] == "cypher-shell" {
		return `OCI runtime exec failed: exec failed: unable to start container process: exec: "cypher-shell": executable file not found in $PATH`,
			errors.New("exit status 126")
	}
	return "", nil
}

func (b *editionProbeFailureBackend) Stop(services ...string) error {
	b.stopped = append(b.stopped, services...)
	return nil
}

// TestBackupStopsWhenNeo4jEditionIsUnknown pins that a backup whose edition
// query fails ends with an error before any service is stopped, instead of
// assuming Community and stopping Infrahub for an offline dump.
func TestBackupStopsWhenNeo4jEditionIsUnknown(t *testing.T) {
	creates := map[string]func(iops *InfrahubOps) error{
		"tarball": func(iops *InfrahubOps) error {
			return iops.CreateBackup(true, "all", false, false, false, 0, false, false, "")
		},
		"plakar": func(iops *InfrahubOps) error {
			return iops.CreatePlakarBackup(true, "all", false, 0, false)
		},
	}

	for name, create := range creates {
		t.Run(name, func(t *testing.T) {
			backend := &editionProbeFailureBackend{bareBackend: bareBackend{name: "kubernetes"}}
			iops := newNeo4jTestOps(backend)
			iops.config.BackupDir = t.TempDir()
			iops.config.Plakar.RepoPath = t.TempDir()
			iops.config.Plakar.CacheDir = t.TempDir()

			err := create(iops)
			if err == nil {
				t.Fatal("backup succeeded although the Neo4j edition could not be determined")
			}
			if !strings.Contains(err.Error(), "could not determine the Neo4j edition") {
				t.Errorf("err = %v, want it to say the Neo4j edition could not be determined", err)
			}
			if !strings.Contains(err.Error(), "executable file not found") {
				t.Errorf("err = %v, want it to carry the failed query's output", err)
			}
			if len(backend.stopped) > 0 {
				t.Errorf("services %v were stopped, want none stopped when the edition is unknown", backend.stopped)
			}
		})
	}
}
