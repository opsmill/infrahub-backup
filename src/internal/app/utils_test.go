package app

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// writeTarFile creates an uncompressed tar at tarPath with the given entries.
// Each entry is a pair of (name, content); directories use empty content.
func writeTarFile(t *testing.T, tarPath string, entries []struct{ name, content string }) {
	t.Helper()
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	for _, e := range entries {
		if e.content == "" && (len(e.name) == 0 || e.name[len(e.name)-1] == '/') {
			// Directory entry
			if err := tw.WriteHeader(&tar.Header{
				Name:     e.name,
				Typeflag: tar.TypeDir,
				Mode:     0755,
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			// File entry
			if err := tw.WriteHeader(&tar.Header{
				Name:     e.name,
				Typeflag: tar.TypeReg,
				Mode:     0644,
				Size:     int64(len(e.content)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestExtractUncompressedTar_StripComponents(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test.tar")
	destDir := filepath.Join(tmpDir, "output")

	writeTarFile(t, tarPath, []struct{ name, content string }{
		{"infrahubops/", ""},
		{"infrahubops/file1.txt", "hello"},
		{"infrahubops/subdir/", ""},
		{"infrahubops/subdir/file2.txt", "world"},
	})

	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := extractUncompressedTar(tarPath, destDir, 1); err != nil {
		t.Fatalf("extractUncompressedTar failed: %v", err)
	}

	// Verify files are extracted with prefix stripped
	data, err := os.ReadFile(filepath.Join(destDir, "file1.txt"))
	if err != nil {
		t.Fatalf("file1.txt not found: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file1.txt content = %q, want %q", data, "hello")
	}

	data, err = os.ReadFile(filepath.Join(destDir, "subdir", "file2.txt"))
	if err != nil {
		t.Fatalf("subdir/file2.txt not found: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("subdir/file2.txt content = %q, want %q", data, "world")
	}
}

func TestExtractUncompressedTar_NoStrip(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test.tar")
	destDir := filepath.Join(tmpDir, "output")

	writeTarFile(t, tarPath, []struct{ name, content string }{
		{"file1.txt", "content1"},
		{"dir/", ""},
		{"dir/file2.txt", "content2"},
	})

	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := extractUncompressedTar(tarPath, destDir, 0); err != nil {
		t.Fatalf("extractUncompressedTar failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "file1.txt"))
	if err != nil {
		t.Fatalf("file1.txt not found: %v", err)
	}
	if string(data) != "content1" {
		t.Errorf("file1.txt content = %q, want %q", data, "content1")
	}

	data, err = os.ReadFile(filepath.Join(destDir, "dir", "file2.txt"))
	if err != nil {
		t.Fatalf("dir/file2.txt not found: %v", err)
	}
	if string(data) != "content2" {
		t.Errorf("dir/file2.txt content = %q, want %q", data, "content2")
	}
}

func TestExtractUncompressedTar_ZipSlipPrevention(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test.tar")
	destDir := filepath.Join(tmpDir, "output")

	writeTarFile(t, tarPath, []struct{ name, content string }{
		{"infrahubops/../../../etc/passwd", "malicious"},
	})

	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	err := extractUncompressedTar(tarPath, destDir, 1)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}
