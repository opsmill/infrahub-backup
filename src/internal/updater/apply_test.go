package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func serveBytes(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApplyUpdateReplacesBinary(t *testing.T) {
	newContent := []byte("#!/bin/sh\necho NEW VERSION\n")
	sum := sha256.Sum256(newContent)
	srv := serveBytes(t, newContent)

	dir := t.TempDir()
	target := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(target, []byte("OLD VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}

	asset := Asset{Name: "fakebin", BrowserDownloadURL: srv.URL}
	if err := applyUpdate(context.Background(), asset, hex.EncodeToString(sum[:]), target); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != string(newContent) {
		t.Errorf("target not replaced; got %q", string(got))
	}
}

func TestApplyUpdateChecksumMismatchKeepsOriginal(t *testing.T) {
	newContent := []byte("payload that will fail verification")
	srv := serveBytes(t, newContent)

	dir := t.TempDir()
	target := filepath.Join(dir, "fakebin")
	original := []byte("OLD VERSION STILL HERE")
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}

	wrong := sha256.Sum256([]byte("something else entirely"))
	asset := Asset{Name: "fakebin", BrowserDownloadURL: srv.URL}
	err := applyUpdate(context.Background(), asset, hex.EncodeToString(wrong[:]), target)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}

	got, _ := os.ReadFile(target)
	if string(got) != string(original) {
		t.Errorf("original binary was modified on failed update: got %q", string(got))
	}
}
