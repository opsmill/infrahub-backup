package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeRelease wires an httptest server that emulates the GitHub Releases API
// plus asset downloads for the running platform, and points the updater at a
// temp fake binary. It returns the temp binary path and the new payload bytes.
func fakeRelease(t *testing.T, latestTag string, payload []byte) string {
	t.Helper()

	// Neutralize container detection so tests are hermetic inside CI containers.
	oldDocker, oldCgroup := dockerEnvPath, cgroupPath
	dockerEnvPath = filepath.Join(t.TempDir(), "no-dockerenv")
	cgroupPath = filepath.Join(t.TempDir(), "no-cgroup")
	t.Cleanup(func() { dockerEnvPath, cgroupPath = oldDocker, oldCgroup })

	assetName := platformFor("infrahub-backup", runtime.GOOS, runtime.GOARCH).AssetName
	sum := sha256.Sum256(payload)
	sumsBody := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName)

	mux := http.NewServeMux()
	var base string
	releaseJSON := func(tag string) string {
		return fmt.Sprintf(`{"tag_name":%q,"html_url":"http://example/%s","assets":[`+
			`{"name":%q,"browser_download_url":"%s/dl/asset"},`+
			`{"name":"SHA256SUMS","browser_download_url":"%s/dl/sums"}]}`,
			tag, tag, assetName, base, base)
	}
	mux.HandleFunc("/repos/opsmill/infrahub-backup/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(releaseJSON(latestTag)))
	})
	mux.HandleFunc("/repos/opsmill/infrahub-backup/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := filepath.Base(r.URL.Path)
		_, _ = w.Write([]byte(releaseJSON(tag)))
	})
	mux.HandleFunc("/dl/asset", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sumsBody)) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "infrahub-backup")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := osExecutable
	osExecutable = func() (string, error) { return target, nil }
	t.Cleanup(func() { osExecutable = old })

	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return target
}

func alwaysProceed(from, to string) (bool, error) { return true, nil }

func TestUpdateHappyPath(t *testing.T) {
	payload := []byte("NEW BINARY CONTENT")
	target := fakeRelease(t, "v1.8.0", payload)

	res, err := Update(context.Background(), Options{BinaryName: "infrahub-backup", CurrentVersion: "v1.0.0"}, alwaysProceed)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Action != ActionUpdated || res.ToVersion != "v1.8.0" {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(payload) {
		t.Errorf("binary not replaced: %q", string(got))
	}
}

func TestUpdateAlreadyCurrent(t *testing.T) {
	target := fakeRelease(t, "v1.8.0", []byte("NEW"))
	res, err := Update(context.Background(), Options{BinaryName: "infrahub-backup", CurrentVersion: "v1.8.0"}, alwaysProceed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionAlreadyCurrent {
		t.Errorf("got action %q", res.Action)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Error("binary changed on no-op update")
	}
}

func TestCheckIsReadOnly(t *testing.T) {
	target := fakeRelease(t, "v1.8.0", []byte("NEW"))
	res, err := Check(context.Background(), Options{BinaryName: "infrahub-backup", CurrentVersion: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionAvailable || res.ToVersion != "v1.8.0" {
		t.Errorf("unexpected check result: %+v", res)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Error("check must not modify the binary")
	}
}

func TestUpdateCancelled(t *testing.T) {
	target := fakeRelease(t, "v1.8.0", []byte("NEW"))
	decline := func(from, to string) (bool, error) { return false, nil }
	res, err := Update(context.Background(), Options{BinaryName: "infrahub-backup", CurrentVersion: "v1.0.0"}, decline)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRefused || res.RefusedReason != "update cancelled" {
		t.Errorf("unexpected result: %+v", res)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Error("declined update must not modify the binary")
	}
}

func TestUpdateVersionPinDowngrade(t *testing.T) {
	payload := []byte("PINNED 1.7.2")
	target := fakeRelease(t, "v1.8.0", payload)
	// Installed is newer than the pinned target → downgrade should still proceed.
	res, err := Update(context.Background(), Options{BinaryName: "infrahub-backup", CurrentVersion: "v1.8.0", TargetVersion: "v1.7.2"}, alwaysProceed)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Action != ActionUpdated || res.ToVersion != "v1.7.2" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got, _ := os.ReadFile(target); string(got) != string(payload) {
		t.Errorf("pinned downgrade not applied: %q", string(got))
	}
}

func TestProceedGating(t *testing.T) {
	// assumeYes always proceeds.
	ok, err := Proceed(true, "v1", "v2")
	if err != nil || !ok {
		t.Errorf("assumeYes: ok=%v err=%v", ok, err)
	}
	// Non-interactive without assumeYes → ErrNonInteractive.
	oldCheck := interactiveCheck
	interactiveCheck = func() bool { return false }
	defer func() { interactiveCheck = oldCheck }()
	if _, err := Proceed(false, "v1", "v2"); err != ErrNonInteractive {
		t.Errorf("expected ErrNonInteractive, got %v", err)
	}
}

func TestRefusalReasons(t *testing.T) {
	if r := refusalReason(InstalledBinary{InstallMethod: InstallHomebrew, Writable: true}); r == "" {
		t.Error("homebrew should be refused")
	}
	if r := refusalReason(InstalledBinary{InstallMethod: InstallDirect, Writable: false, Path: "/x"}); r == "" {
		t.Error("non-writable should be refused")
	}
	if r := refusalReason(InstalledBinary{InstallMethod: InstallDirect, Writable: true}); r != "" {
		t.Errorf("eligible install should not be refused, got %q", r)
	}
}
