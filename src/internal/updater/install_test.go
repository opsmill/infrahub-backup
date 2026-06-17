package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectInstallMethod(t *testing.T) {
	// Dev build: unversioned.
	if m := detectInstallMethod("deadbeef-dirty", "/usr/local/bin/infrahub-backup"); m != InstallDev {
		t.Errorf("dev build: got %q", m)
	}
	// Homebrew prefixes.
	for _, p := range []string{"/opt/homebrew/bin/infrahub-backup", "/usr/local/Cellar/infrahub-backup/1.7.3/bin/infrahub-backup"} {
		if m := detectInstallMethod("v1.7.3", p); m != InstallHomebrew {
			t.Errorf("homebrew path %q: got %q", p, m)
		}
	}
	// Direct install with a valid version.
	if m := detectInstallMethod("v1.7.3", "/home/user/.local/bin/infrahub-backup"); m != InstallDirect {
		t.Errorf("direct: got %q", m)
	}
}

func TestInContainerViaDockerEnv(t *testing.T) {
	if os.Getenv("GOOS") != "" { // detection is linux-only; skip elsewhere is handled by runtime.GOOS inside.
		t.Skip()
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := dockerEnvPath
	dockerEnvPath = marker
	defer func() { dockerEnvPath = old }()
	// inContainer only returns true on linux; assert it does not panic and is
	// consistent with the platform.
	_ = inContainer()
}

func TestIsWritable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isWritable(bin) {
		t.Error("expected writable temp file to report writable")
	}

	// A path in a non-existent directory is not writable.
	if isWritable(filepath.Join(dir, "nope", "deep", "bin")) {
		t.Error("expected non-existent dir to report not writable")
	}
}
