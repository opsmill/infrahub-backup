package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// neo4jAuthFailure is what cypher-shell prints when Neo4j rejects the login.
const neo4jAuthFailure = "The client is unauthorized due to authentication failure."

// serverStoppedBackend is a deployment whose infrahub-server is stopped while the database
// keeps running with a password that is not the tool's default: the state a restore that
// failed partway leaves behind, and the one in which the edition probe of #168 failed.
// Reading the credentials from infrahub-server fails, and Neo4j rejects the default
// credentials the tool falls back to.
type serverStoppedBackend struct {
	bareBackend

	// calls is every command run in a container, joined by spaces.
	calls []string
	// stopped is every service the run stopped.
	stopped []string
}

func newServerStoppedBackend() *serverStoppedBackend {
	return &serverStoppedBackend{bareBackend: bareBackend{name: "docker"}}
}

func (b *serverStoppedBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	b.calls = append(b.calls, strings.Join(command, " "))

	switch {
	case service == "infrahub-server":
		return `service "infrahub-server" is not running`, errors.New("exit status 1")
	case command[0] == "cypher-shell":
		return neo4jAuthFailure, errors.New("exit status 1")
	}

	return "", nil
}

func (b *serverStoppedBackend) Stop(services ...string) error {
	b.stopped = append(b.stopped, services...)

	return nil
}

var _ EnvironmentBackend = (*serverStoppedBackend)(nil)

// clearNeo4jCredentialEnv keeps credentials from the test process's environment out of the
// lookup, so the tool has to read them from the deployment.
func clearNeo4jCredentialEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{"INFRAHUB_DB_DATABASE", "INFRAHUB_DB_USERNAME", "INFRAHUB_DB_PASSWORD"} {
		t.Setenv(name, "")
	}
}

// writeMetadataOnlyArchive writes a tarball archive holding only the backup metadata, which
// is everything a restore reads before it resolves the Neo4j edition.
func writeMetadataOnlyArchive(t *testing.T, metadata BackupMetadata) string {
	t.Helper()

	sourceDir := t.TempDir()
	backupDir := filepath.Join(sourceDir, "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("preparing the archive contents = %v, want nil", err)
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal(metadata) = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), body, 0o600); err != nil {
		t.Fatalf("writing the metadata = %v, want nil", err)
	}

	archive := filepath.Join(t.TempDir(), "infrahub_backup_20260818_133244.tar.gz")
	if err := createTarball(archive, sourceDir, "backup/"); err != nil {
		t.Fatalf("createTarball() = %v, want nil", err)
	}

	return archive
}

// TestRestoreBackupReportsAFailedEditionProbe pins #168. With infrahub-server stopped, the
// edition probe could not log in to Neo4j, and the restore refused an Enterprise archive as a
// mismatch with a Community deployment that was in fact Enterprise. A probe that fails must
// be reported as a failed probe, naming the credential lookup and the container it could not
// reach, and before the restore has stopped anything.
func TestRestoreBackupReportsAFailedEditionProbe(t *testing.T) {
	clearNeo4jCredentialEnv(t)
	archive := writeMetadataOnlyArchive(t, BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        "infrahub_backup_20260818_133244",
		Components:      []string{"database"},
		Neo4jEdition:    neo4jEditionEnterprise,
	})

	backend := newServerStoppedBackend()
	iops := NewInfrahubOps()
	iops.backend = backend

	err := iops.RestoreBackup(archive, false, false, 0, "", false, false)
	if err == nil {
		t.Fatal("RestoreBackup() = nil, want the failed edition probe reported")
	}

	msg := err.Error()
	for _, wrong := range []string{"Community", "cannot restore Enterprise backup"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("RestoreBackup() = %q, reports an edition mismatch (%q) for an edition it could not detect", msg, wrong)
		}
	}
	for _, want := range []string{
		"could not detect the Neo4j edition",
		// The probe's own failure, as Neo4j reported it.
		neo4jAuthFailure,
		// The credential lookup that left the probe with the default credentials, and the
		// container it could not reach.
		"default Neo4j credentials",
		"infrahub-server container",
		`service "infrahub-server" is not running`,
		// What the operator can do about it.
		"INFRAHUB_DB_PASSWORD",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("RestoreBackup() = %q, want it to contain %q", msg, want)
		}
	}

	// The error quotes what the probe printed, never the command line that carries the password.
	if strings.Contains(msg, "-p"+defaultNeo4jPassword) {
		t.Errorf("RestoreBackup() = %q, contains the Neo4j password", msg)
	}

	// Nothing about the deployment changed: the probe runs before any service is stopped or
	// any data is wiped.
	if len(backend.stopped) > 0 {
		t.Errorf("RestoreBackup() stopped %v before reporting the failed probe", backend.stopped)
	}
	if indexOf(backend.calls, "find /var/lib/rabbitmq") >= 0 {
		t.Errorf("RestoreBackup() wiped the message queue before reporting the failed probe; calls: %v", backend.calls)
	}
}

// TestResolveRestoreEdition covers the restore method chosen for each combination of archive
// edition and detected edition. An edition the probe could not detect is unknown, not
// Community: only a Community archive can still be restored, because the community restore
// method works on either edition and does not log in to Neo4j.
func TestResolveRestoreEdition(t *testing.T) {
	probeErr := errors.New("failed to query neo4j edition: " + neo4jAuthFailure + " (exit status 1)")

	tests := []struct {
		name         string
		detected     string
		detectionErr error
		backup       string
		want         string
		// wantErr is a substring of the expected error; empty means no error.
		wantErr string
	}{
		{
			name:     "enterprise archive on enterprise",
			detected: neo4jEditionEnterprise,
			backup:   neo4jEditionEnterprise,
			want:     neo4jEditionEnterprise,
		},
		{
			name:     "community archive on enterprise uses the community method",
			detected: neo4jEditionEnterprise,
			backup:   neo4jEditionCommunity,
			want:     neo4jEditionCommunity,
		},
		{
			name:     "community archive on community",
			detected: neo4jEditionCommunity,
			backup:   neo4jEditionCommunity,
			want:     neo4jEditionCommunity,
		},
		{
			name:     "enterprise archive on community is a real mismatch",
			detected: neo4jEditionCommunity,
			backup:   neo4jEditionEnterprise,
			wantErr:  "cannot restore Enterprise backup on Community edition Neo4j",
		},
		{
			name:         "enterprise archive on an undetected edition",
			detectionErr: probeErr,
			backup:       neo4jEditionEnterprise,
			wantErr:      "could not detect the Neo4j edition",
		},
		{
			name:         "archive without a recorded edition on an undetected edition",
			detectionErr: probeErr,
			backup:       "",
			wantErr:      "could not detect the Neo4j edition",
		},
		{
			name:         "community archive on an undetected edition uses the community method",
			detectionErr: probeErr,
			backup:       neo4jEditionCommunity,
			want:         neo4jEditionCommunity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := NewNeo4jEditionInfo(tc.detected, tc.detectionErr)

			got, err := info.ResolveRestoreEdition(tc.backup)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveRestoreEdition(%q) = %v, want %q", tc.backup, err, tc.want)
				}
				if got != tc.want {
					t.Errorf("ResolveRestoreEdition(%q) = %q, want %q", tc.backup, got, tc.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("ResolveRestoreEdition(%q) = %q, want an error containing %q", tc.backup, got, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ResolveRestoreEdition(%q) = %q, want it to contain %q", tc.backup, err, tc.wantErr)
			}
			if tc.detectionErr != nil {
				if !errors.Is(err, tc.detectionErr) {
					t.Errorf("ResolveRestoreEdition(%q) = %q, want it to wrap the probe error", tc.backup, err)
				}
				if strings.Contains(err.Error(), "Community") {
					t.Errorf("ResolveRestoreEdition(%q) = %q, names an edition it could not detect", tc.backup, err)
				}
			}
		})
	}
}

// TestEditionProbeNamesTheDefaultCredentialsOnlyWhenUsed checks that a failed edition probe
// blames the credential lookup only when that lookup left the login on the tool's default
// username or password. Credentials set in the environment are used as they are, so the
// lookup failing does not explain a failed login.
func TestEditionProbeNamesTheDefaultCredentialsOnlyWhenUsed(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		// wantDefaults is whether the login fell back to a default username or password.
		wantDefaults bool
	}{
		{
			name:         "no credentials in the environment",
			wantDefaults: true,
		},
		{
			name:         "username and password in the environment",
			env:          map[string]string{"INFRAHUB_DB_USERNAME": "neo4j", "INFRAHUB_DB_PASSWORD": "s3cret"},
			wantDefaults: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearNeo4jCredentialEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			iops := NewInfrahubOps()
			iops.backend = newServerStoppedBackend()

			if err := iops.fetchDatabaseCredentials(); err != nil {
				t.Fatalf("fetchDatabaseCredentials() = %v, want nil", err)
			}
			_, err := iops.detectNeo4jEdition()
			if err == nil {
				t.Fatal("detectNeo4jEdition() = nil, want the rejected login reported")
			}

			msg := err.Error()
			if !strings.Contains(msg, neo4jAuthFailure) {
				t.Errorf("detectNeo4jEdition() = %q, want it to contain what cypher-shell printed", msg)
			}
			if got := strings.Contains(msg, "infrahub-server container"); got != tc.wantDefaults {
				t.Errorf("detectNeo4jEdition() = %q; names the credential lookup = %t, want %t", msg, got, tc.wantDefaults)
			}
		})
	}
}
