package updater

import (
	"strings"
	"testing"
)

func TestParseChecksums(t *testing.T) {
	in := "abc123  infrahub-backup-linux-amd64\n" +
		"DEF456  infrahub-backup-darwin-arm64\n" +
		"\n" +
		"789aaa *infrahub-taskmanager-windows-amd64.exe\n"
	c, err := parseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseChecksums: %v", err)
	}
	if got, _ := c.digestFor("infrahub-backup-linux-amd64"); got != "abc123" {
		t.Errorf("linux digest = %q", got)
	}
	// Lowercased.
	if got, _ := c.digestFor("infrahub-backup-darwin-arm64"); got != "def456" {
		t.Errorf("darwin digest = %q (want lowercased)", got)
	}
	// Binary-mode "*" prefix stripped from filename.
	if got, _ := c.digestFor("infrahub-taskmanager-windows-amd64.exe"); got != "789aaa" {
		t.Errorf("windows digest = %q", got)
	}
	if _, err := c.digestFor("missing-asset"); err == nil {
		t.Error("expected error for missing asset")
	}
}

func TestParseChecksumsErrors(t *testing.T) {
	if _, err := parseChecksums(strings.NewReader("")); err == nil {
		t.Error("expected error for empty SHA256SUMS")
	}
	if _, err := parseChecksums(strings.NewReader("onlyonefield\n")); err == nil {
		t.Error("expected error for malformed line")
	}
}
