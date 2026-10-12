package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withBuildVersion sets the build version a binary's main would have set from
// its ldflags, for the duration of one test. The default workload image is read
// from it when the configuration is built (FR-031), so a test that needs a
// released build's default, or a development build's absence of one, says
// which it is rather than inheriting whatever the test binary was built with.
func withBuildVersion(t *testing.T, v string) {
	t.Helper()

	previous := version
	SetVersion(v)
	t.Cleanup(func() { SetVersion(previous) })
}

// TestPublishedImageTag is the definition of "development build" FR-031 turns
// on, as a table: a release sets main.version from its git tag ("v2.3.0") and
// the release workflow pushes the image under that tag without its "v"
// ("2.3.0"), so exactly the release-shaped versions have a tag, and it is that
// one.
func TestPublishedImageTag(t *testing.T) {
	tests := []struct {
		version string
		wantTag string
		wantOK  bool
	}{
		{version: "v2.3.0", wantTag: "2.3.0", wantOK: true},
		// `make build VERSION=2.3.0` names the same release.
		{version: "2.3.0", wantTag: "2.3.0", wantOK: true},
		// A published pre-release is a release too: the workflow runs on it.
		{version: "v2.4.0-rc.1", wantTag: "2.4.0-rc.1", wantOK: true},
		// A plain `go build` or `make build` leaves the version empty.
		{version: "", wantOK: false},
		// A commit hash, as BuildRevision falls back to.
		{version: "ea1d73b", wantOK: false},
		{version: "ea1d73b4c0ffee-dirty", wantOK: false},
		// `git describe` output and build metadata: not a tag anything was
		// pushed under.
		{version: "v2.3.0-4-gea1d73b", wantOK: false},
		{version: "v2.3.0-4-gea1d73b-dirty", wantOK: false},
		{version: "v2.3.0-dirty", wantOK: false},
		{version: "v2.3.0+dirty", wantOK: false},
		// semver reads "v2.3" as "v2.3.0", but no image was pushed as "2.3".
		{version: "v2.3", wantOK: false},
		{version: "dev", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.version), func(t *testing.T) {
			tag, ok := publishedImageTag(tt.version)
			if ok != tt.wantOK || tag != tt.wantTag {
				t.Errorf("publishedImageTag(%q) = (%q, %v), want (%q, %v)", tt.version, tag, ok, tt.wantTag, tt.wantOK)
			}
		})
	}
}

// TestDefaultExternalDBImageFollowsTheBuildVersion is T165: the workload runs
// this tool's own image at the running binary's version, from the registry the
// release workflow publishes to, and a development build has no default at all
// rather than a guessed one.
func TestDefaultExternalDBImageFollowsTheBuildVersion(t *testing.T) {
	t.Run("a release runs its own image", func(t *testing.T) {
		withBuildVersion(t, "v2.3.0")

		const want = "registry.opsmill.io/opsmill/infrahub-backup:2.3.0"
		if got := defaultExternalDBImage(); got != want {
			t.Errorf("defaultExternalDBImage() = %q, want %q", got, want)
		}
		if got := NewInfrahubOps().Config().ExternalDB.Image; got != want {
			t.Errorf("a new configuration's Image = %q, want %q", got, want)
		}
	})

	t.Run("a development build has no default", func(t *testing.T) {
		withBuildVersion(t, "")

		if got := defaultExternalDBImage(); got != "" {
			t.Errorf("defaultExternalDBImage() = %q on a development build, want empty: no image was published for it", got)
		}
		if got := NewInfrahubOps().Config().ExternalDB.Image; got != "" {
			t.Errorf("a new configuration's Image = %q on a development build, want empty", got)
		}
	})
}

// writeRecordingKubectl installs a kubectl that appends its arguments to log
// and fails, and returns the log's path. Failing is deliberate: a stub that
// succeeded would let a run that should have been refused go on to read its
// own invented answers.
func writeRecordingKubectl(t *testing.T) string {
	t.Helper()

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "kubectl-calls")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 1\n", log)
	stub := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the kubectl stub = %v, want nil", err)
	}

	// A positive control: the assertion below is "the log is empty", which a
	// stub that could not write its log would satisfy vacuously.
	if err := exec.Command(stub, "version").Run(); err == nil {
		t.Fatal("the kubectl stub exited 0, want it to fail")
	}
	recorded, err := os.ReadFile(log)
	if err != nil || strings.TrimSpace(string(recorded)) != "version" {
		t.Fatalf("the kubectl stub did not record its own call (log %q, err %v); the test could not observe a call", recorded, err)
	}
	if err := os.Remove(log); err != nil {
		t.Fatalf("resetting the kubectl log = %v, want nil", err)
	}

	t.Setenv("PATH", bin)

	return log
}

// TestDevelopmentBuildIsRefusedBeforeAnyKubectlCall is FR-031's refusal,
// asserted where it has to land: after the gate has found an external
// database, and before the run has spent a single kubectl call — not the
// reaper's list and delete, not a create. Both directions are driven through
// their real gate, because the refusal is only worth having if neither a
// backup nor a restore can reach a workload without passing it.
func TestDevelopmentBuildIsRefusedBeforeAnyKubectlCall(t *testing.T) {
	neo4jExternal := deploymentQueries{
		locate: func(service string) (EndpointLocation, error) {
			if service == serviceNeo4j {
				return EndpointLocationExternal, nil
			}

			return EndpointLocationInternal, nil
		},
		discover: func(string) error { return nil },
	}

	runs := []struct {
		name string
		run  func(iops *InfrahubOps) error
	}{
		{
			name: "backup",
			run: func(iops *InfrahubOps) error {
				return iops.prepareDatabaseCaptureWith(neo4jExternal, true)
			},
		},
		{
			name: "restore",
			run: func(iops *InfrahubOps) error {
				// Authorised, so the only thing left to refuse it is the image.
				iops.config.ExternalDB.Restore = ExternalRestoreAuth{Source: ExternalRestoreAuthFlag}

				return iops.prepareDatabaseRestoreWith(neo4jExternal, true)
			},
		},
	}

	for _, tt := range runs {
		t.Run(tt.name, func(t *testing.T) {
			withBuildVersion(t, "")
			log := writeRecordingKubectl(t)

			iops := NewInfrahubOps()
			iops.backend = newTestKubernetesBackend()

			err := tt.run(iops)
			if err == nil {
				t.Fatal("the gate returned nil on a development build with no --external-db-image, want the FR-031 refusal")
			}
			for _, want := range []string{"--external-db-image", "development build", "Nothing was created or stopped"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}

			if calls, readErr := os.ReadFile(log); readErr == nil && len(calls) > 0 {
				t.Errorf("kubectl was called before the refusal:\n%s\nwant no call at all: the refusal must precede the reaper and every create", calls)
			}
		})
	}
}

// TestExternalDBImageFlagLiftsTheDevelopmentBuildRefusal is the other half of
// FR-031's "unless that flag is set", and the release-build case: with an
// image to run there is nothing to refuse, and an image explicitly emptied on
// a release is refused for what it is rather than called a development build.
func TestExternalDBImageFlagLiftsTheDevelopmentBuildRefusal(t *testing.T) {
	t.Run("a development build given the flag passes", func(t *testing.T) {
		withBuildVersion(t, "")
		cfg := &Configuration{ExternalDB: ExternalDBConfig{Image: "registry.example.net/infrahub-backup:dev"}}

		if err := refuseUnpublishedExternalDBImage(cfg); err != nil {
			t.Errorf("refuseUnpublishedExternalDBImage() = %v, want nil: the operator chose an image", err)
		}
	})

	t.Run("a release passes on its default", func(t *testing.T) {
		withBuildVersion(t, "v2.3.0")

		if err := refuseUnpublishedExternalDBImage(NewInfrahubOps().Config()); err != nil {
			t.Errorf("refuseUnpublishedExternalDBImage() = %v, want nil on a release's own default", err)
		}
	})

	t.Run("a release given an empty flag names the flag, not a development build", func(t *testing.T) {
		withBuildVersion(t, "v2.3.0")

		err := refuseUnpublishedExternalDBImage(&Configuration{})
		if err == nil {
			t.Fatal("refuseUnpublishedExternalDBImage() = nil with no image, want a refusal")
		}
		if !strings.Contains(err.Error(), "--external-db-image") || !strings.Contains(err.Error(), "empty value") {
			t.Errorf("err = %v, want it to say the flag was given an empty value", err)
		}
		if strings.Contains(err.Error(), "development build") {
			t.Errorf("err = %v, want no mention of a development build on a release", err)
		}
	})
}
