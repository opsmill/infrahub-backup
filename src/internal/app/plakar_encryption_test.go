package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
)

// newTestPlakarConfig returns a PlakarConfig pointing at fresh temp repo + cache
// directories so each test is isolated.
func newTestPlakarConfig(t *testing.T) *PlakarConfig {
	t.Helper()
	return &PlakarConfig{
		RepoPath: filepath.Join(t.TempDir(), "repo"),
		CacheDir: t.TempDir(),
	}
}

// writeTestSnapshot opens (creating if needed) the repo described by cfg and
// writes one snapshot whose single file contains data, tagged with tags.
func writeTestSnapshot(t *testing.T, cfg *PlakarConfig, name string, data []byte, tags []string) {
	t.Helper()
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	repo, err := openOrCreateRepo(kctx, cfg)
	if err != nil {
		t.Fatalf("openOrCreateRepo: %v", err)
	}
	defer closeRepo(repo)

	imp := NewMemoryImporter(kctx.Hostname, "/secret.json", data)
	src, err := snapshot.NewSource(context.Background(), imp)
	if err != nil {
		t.Fatalf("snapshot.NewSource: %v", err)
	}
	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, &snapshot.BuilderOptions{
		Name: name,
		Tags: tags,
	})
	if err != nil {
		t.Fatalf("snapshot.Create: %v", err)
	}
	defer builder.Close()
	if err := builder.Backup(src); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := builder.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// repoContainsBytes reports whether any file under dir contains marker verbatim.
func repoContainsBytes(t *testing.T, dir string, marker []byte) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, marker) {
			found = true
		}
		return nil
	})
	return found
}

// randomMarker returns n high-entropy bytes. High entropy is intentional: an
// incompressible payload is stored as a literal block in a plaintext repo (so
// the control assertion is reliable) yet must be absent from an encrypted one.
func randomMarker(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// T013 / SC-001 / G1: an encrypted repo's stored bytes must not reveal the
// backed-up content; a plaintext repo (the control) must.
func TestEncryptedRepoHidesPlaintext(t *testing.T) {
	marker := randomMarker(t, 1024)

	// Control: a plaintext repo stores the marker verbatim.
	plain := newTestPlakarConfig(t)
	writeTestSnapshot(t, plain, "control", marker, nil)
	if !repoContainsBytes(t, plain.RepoPath, marker) {
		t.Fatal("control failed: plaintext repo does not contain the marker — test cannot prove encryption hides it")
	}

	// Encrypted repo must NOT contain the marker.
	enc := newTestPlakarConfig(t)
	enc.Encrypt = true
	enc.Passphrase = "correct horse battery staple"
	writeTestSnapshot(t, enc, "secret", marker, nil)
	if repoContainsBytes(t, enc.RepoPath, marker) {
		t.Fatal("SC-001 violated: encrypted repo bytes contain the plaintext marker")
	}
}

// T021 (data layer) / SC-002: an encrypted backup→restore round-trip recovers
// the exact content with the correct passphrase — proving the data is genuinely
// encrypted at rest yet decryptable with the key (the engine secret round-trips).
func TestEncryptedRoundTripInProcess(t *testing.T) {
	cfg := newTestPlakarConfig(t)
	cfg.Encrypt = true
	cfg.Passphrase = "round-trip-passphrase-1"
	payload := randomMarker(t, 2048)
	writeTestSnapshot(t, cfg, "rt", payload, nil)

	// Open with the key, load the (only) snapshot, export it to a temp dir.
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)
	repo, err := openRepo(kctx, cfg)
	if err != nil {
		t.Fatalf("openRepo with key: %v", err)
	}
	defer closeRepo(repo)

	var mac objects.MAC
	found := false
	for m, lerr := range repo.ListSnapshots() {
		if lerr != nil {
			t.Fatalf("list: %v", lerr)
		}
		mac = m
		found = true
	}
	if !found {
		t.Fatal("no snapshot found in encrypted repo")
	}
	snap, err := snapshot.Load(repo, mac)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	defer snap.Close()

	outDir := t.TempDir()
	exp, err := exporter.NewExporter(kctx, connectorOptions(kctx), map[string]string{"location": "fs://" + outDir})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	defer exp.Close(kctx.Context)
	if err := snap.Export(exp, "/", &snapshot.ExportOptions{SkipPermissions: true}); err != nil {
		t.Fatalf("export: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "secret.json"))
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("restored content does not match the original — decryption round-trip failed")
	}
}

// Gap-sweep fix: --encrypt pointed at an EXISTING plaintext repo must refuse
// (encryption is fixed at create, FR-008) rather than silently appending
// plaintext with only a warning.
func TestEncryptExistingPlaintextRepoRefused(t *testing.T) {
	cfg := newTestPlakarConfig(t) // plaintext repo
	writeTestSnapshot(t, cfg, "plain", randomMarker(t, 128), nil)

	// Re-target the same repo with --encrypt + a valid passphrase.
	enc := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir(), Encrypt: true, Passphrase: "a-valid-passphrase-1"}
	kctx, err := initPlakarContext(enc)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)
	if _, err := openOrCreateRepo(kctx, enc); !errors.Is(err, errEncryptExistingPlaintextRepo) {
		t.Fatalf("want errEncryptExistingPlaintextRepo, got %v", err)
	}
}

// T013 / VR-1 / FR-006: --encrypt with no passphrase is refused before any repo
// is created.
func TestEncryptWithoutPassphraseRefused(t *testing.T) {
	cfg := newTestPlakarConfig(t)
	cfg.Encrypt = true // no passphrase

	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	_, err = openOrCreateRepo(kctx, cfg)
	if !errors.Is(err, errEncryptWithoutPassphrase) {
		t.Fatalf("want errEncryptWithoutPassphrase, got %v", err)
	}
	// And nothing was created.
	if _, statErr := os.Stat(cfg.RepoPath); statErr == nil {
		t.Fatal("repository directory was created despite the refusal")
	}
}

// Regression (F1): resolvePassphrase must normalize the env var the same way it
// normalizes a file and the way the runner normalizes stdin (firstLine), so the
// host-stamped canary and the runner-derived data key always agree. A trailing
// newline on the env value must not change the derived passphrase.
func TestResolvePassphraseNormalizesEnv(t *testing.T) {
	t.Setenv(passphraseEnvVar, "secret-passphrase-123\n")
	got, err := resolvePassphrase("")
	if err != nil {
		t.Fatalf("resolvePassphrase: %v", err)
	}
	if got != "secret-passphrase-123" {
		t.Fatalf("env passphrase not first-line normalized: got %q", got)
	}

	// A file with a trailing newline must resolve to the identical value.
	f := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(f, []byte("secret-passphrase-123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(passphraseEnvVar, "")
	fromFile, err := resolvePassphrase(f)
	if err != nil {
		t.Fatalf("resolvePassphrase(file): %v", err)
	}
	if fromFile != got {
		t.Fatalf("env (%q) and file (%q) passphrases disagree after normalization", got, fromFile)
	}
}

// T013 / VR-6 / FR-013: a passphrase shorter than the minimum is rejected.
func TestValidatePassphraseMinLength(t *testing.T) {
	if err := validatePassphrase("short"); err == nil {
		t.Fatal("want error for an 11-or-fewer character passphrase")
	}
	if err := validatePassphrase("exactly12chr"); err != nil { // 12 chars
		t.Fatalf("12-char passphrase should pass, got %v", err)
	}
}

// VR-7: --encrypt-key with the plakar backend is rejected, never silently ignored.
func TestEncryptKeyRejectedOnPlakar(t *testing.T) {
	t.Setenv(passphraseEnvVar, "") // hermetic: don't read an ambient passphrase
	iops := NewInfrahubOps()
	err := iops.PreparePlakarEncryption(false, "/path/to/key.pub", "")
	if !errors.Is(err, errEncryptKeyOnPlakar) {
		t.Fatalf("want errEncryptKeyOnPlakar, got %v", err)
	}
}

// PreparePlakarEncryption must refuse --encrypt without a passphrase and reject a
// too-short one, before any repository work (FR-006, FR-013).
func TestPreparePlakarEncryptionValidation(t *testing.T) {
	t.Setenv(passphraseEnvVar, "")

	iops := NewInfrahubOps()
	if err := iops.PreparePlakarEncryption(true, "", ""); !errors.Is(err, errEncryptWithoutPassphrase) {
		t.Fatalf("want errEncryptWithoutPassphrase, got %v", err)
	}

	t.Setenv(passphraseEnvVar, "tooshort") // 8 chars
	iops = NewInfrahubOps()
	if err := iops.PreparePlakarEncryption(true, "", ""); err == nil {
		t.Fatal("want too-short error, got nil")
	}

	t.Setenv(passphraseEnvVar, "a-long-enough-passphrase")
	iops = NewInfrahubOps()
	if err := iops.PreparePlakarEncryption(true, "", ""); err != nil {
		t.Fatalf("valid passphrase should pass, got %v", err)
	}
	if !iops.Config().Plakar.Encrypt || iops.Config().Plakar.Passphrase != "a-long-enough-passphrase" {
		t.Fatal("encrypt flag / passphrase not stored on config")
	}
}

// T016 / SC-003 / SC-006 / G2: opening an encrypted repo with a wrong or absent
// passphrase fails fast via the canary, with clear distinct errors.
func TestOpenEncryptedWrongOrAbsentPassphrase(t *testing.T) {
	cfg := newTestPlakarConfig(t)
	cfg.Encrypt = true
	cfg.Passphrase = "the-right-passphrase"
	writeTestSnapshot(t, cfg, "secret", randomMarker(t, 256), nil)

	// Wrong passphrase → errWrongPassphrase.
	wrong := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir(), Passphrase: "the-wrong-passphrase"}
	kctx, err := initPlakarContext(wrong)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	if _, err := openRepo(kctx, wrong); !errors.Is(err, errWrongPassphrase) {
		closePlakarContext(kctx)
		t.Fatalf("want errWrongPassphrase, got %v", err)
	}
	closePlakarContext(kctx)

	// Absent passphrase → errEncryptedRepoNeedsPassphrase.
	none := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir()}
	kctx2, err := initPlakarContext(none)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx2)
	if _, err := openRepo(kctx2, none); !errors.Is(err, errEncryptedRepoNeedsPassphrase) {
		t.Fatalf("want errEncryptedRepoNeedsPassphrase, got %v", err)
	}
}

// T019: an encrypted repo lists its backup groups with the key and fails clearly
// without it. Listing is the openRepo + collectBackupGroups path.
func TestListEncryptedRepo(t *testing.T) {
	cfg := newTestPlakarConfig(t)
	cfg.Encrypt = true
	cfg.Passphrase = "list-me-please-securely"
	tags := buildSnapshotTags(&BackupMetadata{Components: []string{ComponentMetadata}}, ComponentMetadata, "20260630_000000", StatusComplete)
	writeTestSnapshot(t, cfg, "metadata", randomMarker(t, 128), tags)

	// With the key: groups are readable.
	ok := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir(), Passphrase: cfg.Passphrase}
	kctx, err := initPlakarContext(ok)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	repo, err := openRepo(kctx, ok)
	if err != nil {
		closePlakarContext(kctx)
		t.Fatalf("open with key: %v", err)
	}
	groups, err := collectBackupGroups(repo)
	if err != nil {
		closeRepo(repo)
		closePlakarContext(kctx)
		t.Fatalf("collectBackupGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].BackupID != "20260630_000000" {
		closeRepo(repo)
		closePlakarContext(kctx)
		t.Fatalf("want 1 group with id 20260630_000000, got %+v", groups)
	}
	closeRepo(repo)
	closePlakarContext(kctx)

	// Without the key: clear failure.
	none := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir()}
	kctx2, err := initPlakarContext(none)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx2)
	if _, err := openRepo(kctx2, none); !errors.Is(err, errEncryptedRepoNeedsPassphrase) {
		t.Fatalf("want errEncryptedRepoNeedsPassphrase, got %v", err)
	}
}

// T022 / SC-005 / VR-3 / G4: plaintext repos are unchanged. A plaintext repo
// opens with no passphrase, AND opening it WITH a passphrase warns and continues
// (no error) rather than failing.
func TestPlaintextRepoUnchanged(t *testing.T) {
	cfg := newTestPlakarConfig(t) // Encrypt=false
	writeTestSnapshot(t, cfg, "plain", randomMarker(t, 128), nil)

	// No passphrase: opens normally.
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	repo, err := openRepo(kctx, cfg)
	if err != nil {
		closePlakarContext(kctx)
		t.Fatalf("plaintext open (no passphrase) failed: %v", err)
	}
	closeRepo(repo)
	closePlakarContext(kctx)

	// Passphrase supplied for a plaintext repo: warn + ignore, no error (VR-3).
	withPass := &PlakarConfig{RepoPath: cfg.RepoPath, CacheDir: t.TempDir(), Passphrase: "ignored-passphrase-here"}
	kctx2, err := initPlakarContext(withPass)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx2)
	repo2, err := openRepo(kctx2, withPass)
	if err != nil {
		t.Fatalf("plaintext open WITH passphrase should warn+continue, got error: %v", err)
	}
	closeRepo(repo2)
}
