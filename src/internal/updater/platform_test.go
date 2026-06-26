package updater

import "testing"

func TestPlatformFor(t *testing.T) {
	cases := []struct {
		binary, goos, goarch, want string
	}{
		{"infrahub-backup", "linux", "amd64", "infrahub-backup-linux-amd64"},
		{"infrahub-backup", "darwin", "arm64", "infrahub-backup-darwin-arm64"},
		{"infrahub-taskmanager", "windows", "amd64", "infrahub-taskmanager-windows-amd64.exe"},
		{"infrahub-taskmanager", "windows", "arm64", "infrahub-taskmanager-windows-arm64.exe"},
		{"infrahub-backup", "linux", "arm64", "infrahub-backup-linux-arm64"},
	}
	for _, c := range cases {
		got := platformFor(c.binary, c.goos, c.goarch).AssetName
		if got != c.want {
			t.Errorf("platformFor(%q,%q,%q).AssetName = %q, want %q", c.binary, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestSelectAsset(t *testing.T) {
	rel := &Release{
		TagName: "v1.8.0",
		Assets: []Asset{
			{Name: "infrahub-backup-linux-amd64", BrowserDownloadURL: "http://x/a"},
			{Name: "SHA256SUMS", BrowserDownloadURL: "http://x/s"},
		},
	}
	pt := platformFor("infrahub-backup", "linux", "amd64")
	a, err := pt.selectAsset(rel)
	if err != nil {
		t.Fatalf("selectAsset: %v", err)
	}
	if a.BrowserDownloadURL != "http://x/a" {
		t.Errorf("got %q", a.BrowserDownloadURL)
	}

	missing := platformFor("infrahub-backup", "plan9", "ppc64")
	if _, err := missing.selectAsset(rel); err == nil {
		t.Error("expected error for unsupported platform")
	}

	if _, err := checksumsAsset(rel); err != nil {
		t.Errorf("checksumsAsset: %v", err)
	}
	if _, err := checksumsAsset(&Release{TagName: "v1.0.0"}); err == nil {
		t.Error("expected error when SHA256SUMS missing")
	}
}
