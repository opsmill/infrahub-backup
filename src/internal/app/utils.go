package app

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"golang.org/x/mod/semver"
)

// Version can be set via SetVersion from main packages using ldflags
var version string

// SetVersion allows main packages to set the version from ldflags
func SetVersion(v string) {
	version = v
}

// publishedImageTag is the tag under which the release workflow published the
// image for build version v, and whether one was published at all.
//
// A release sets main.version from the release's git tag (`make build-all
// VERSION=v2.3.0`), and the image job pushes the same release under the tag
// with its leading "v" stripped (`infrahub-backup:2.3.0`). So the tag is the
// version without its "v", and only a version shaped like a release tag has
// one. A tag written without the "v" is accepted too, because `make build
// VERSION=2.3.0` names the same release.
//
// Anything else is a development build: an empty version (a plain `go build`
// or `make build`, where BuildRevision falls back to the commit hash), a commit
// hash, a `git describe` string, or a version carrying build metadata such as
// `+dirty`. Those have no published image, and guessing one — `latest`, or the
// nearest release — would run tooling whose version nobody chose, in the
// customer's namespace, from a binary that may not match it (FR-031).
// Canonical form is required for the same reason: semver accepts "v2.3" as
// "v2.3.0", but no image was ever pushed as `2.3`.
func publishedImageTag(v string) (string, bool) {
	tagged := v
	if !strings.HasPrefix(tagged, "v") {
		tagged = "v" + tagged
	}
	if !semver.IsValid(tagged) || semver.Canonical(tagged) != tagged {
		return "", false
	}
	// `git describe --tags --dirty` output is valid semver — "v2.3.0-4-gea1d73b"
	// parses as a pre-release of 2.3.0 — but it names a commit after a release,
	// which no image was pushed for. The suffix can follow a pre-release too
	// ("v2.4.0-rc.1-3-gabc1234"), so it is refused wherever it appears. A
	// published pre-release ("v2.4.0-rc.1") passes; only describe's own
	// suffixes are refused.
	if gitDescribeSuffix.MatchString(semver.Prerelease(tagged)) {
		return "", false
	}

	return strings.TrimPrefix(tagged, "v"), true
}

// gitDescribeSuffix matches what `git describe` appends to a tag, anywhere in
// the pre-release part: commits since the tag and an abbreviated hash
// (`-3-gabc1234`), or a dirty marker (`-dirty`). Each must end at an
// identifier boundary, so a pre-release such as "-rc.1" never matches.
var gitDescribeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+([.-]|$)|-dirty([.-]|$)`)

// defaultExternalDBImage is this tool's own image at the running binary's
// version, or empty on a development build, which has none (FR-031). It is
// read when the configuration is built, which every binary does after
// SetVersion.
func defaultExternalDBImage() string {
	tag, ok := publishedImageTag(version)
	if !ok {
		return ""
	}

	return externalDBImageRepository + ":" + tag
}

// Utility functions
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getCurrentDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// fileExists checks if a file exists and is not a directory
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// calculateSHA256 calculates the SHA256 checksum of a file
func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func createTarball(filename, sourceDir, pathInTar string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(filepath.Join(sourceDir, pathInTar), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}

func extractTarball(filename, destDir string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	// Ensure destination directory is absolute for security checks
	destDir, err = filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for destination: %w", err)
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Prevent Zip Slip vulnerability: validate that the target path is within destDir
		target := filepath.Join(destDir, header.Name)
		target = filepath.Clean(target)
		if !isPathWithinDirectory(target, destDir) {
			return fmt.Errorf("illegal file path in archive: %s (attempts to escape destination directory)", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

// extractUncompressedTar extracts an uncompressed tar archive to destDir,
// optionally stripping leading path components (like tar --strip-components).
func extractUncompressedTar(filename, destDir string, stripComponents int) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	tr := tar.NewReader(file)

	destDir, err = filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for destination: %w", err)
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		name := header.Name
		if stripComponents > 0 {
			parts := strings.SplitN(name, "/", stripComponents+1)
			if len(parts) <= stripComponents {
				continue // entry is entirely within the stripped prefix
			}
			name = parts[stripComponents]
			if name == "" {
				continue
			}
		}

		target := filepath.Join(destDir, name)
		target = filepath.Clean(target)
		if !isPathWithinDirectory(target, destDir) {
			return fmt.Errorf("illegal file path in archive: %s (attempts to escape destination directory)", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

// isPathWithinDirectory checks if path is within dir (prevents directory traversal attacks)
func isPathWithinDirectory(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	// If relative path starts with "..", it's trying to escape the directory
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func BuildRevision() string {
	// Use ldflags-set version if available
	if version != "" {
		return version
	}

	// Fall back to git commit hash from build info
	info, _ := debug.ReadBuildInfo()
	var rev = "<none>"
	var dirty = ""
	for _, v := range info.Settings {
		if v.Key == "vcs.revision" {
			rev = v.Value
		}
		if v.Key == "vcs.modified" {
			if v.Value == "true" {
				dirty = "-dirty"
			} else {
				dirty = ""
			}
		}
	}
	return rev + dirty
}

func readEmbeddedScript(name string) ([]byte, error) {
	return scriptsFS.ReadFile("scripts/" + name)
}
