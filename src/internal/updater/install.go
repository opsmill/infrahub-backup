package updater

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Testable seams.
var (
	dockerEnvPath = "/.dockerenv"
	cgroupPath    = "/proc/1/cgroup"
	osExecutable  = os.Executable
)

// inspectInstall resolves the running executable and classifies how it was
// installed, plus whether it can be replaced in place.
func inspectInstall(version string) (InstalledBinary, error) {
	path, err := resolveExecutable()
	if err != nil {
		return InstalledBinary{}, err
	}
	return InstalledBinary{
		Version:       version,
		Path:          path,
		Writable:      isWritable(path),
		InstallMethod: detectInstallMethod(version, path),
	}, nil
}

// resolveExecutable returns the absolute, symlink-resolved path of the running
// binary.
func resolveExecutable() (string, error) {
	exe, err := osExecutable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// detectInstallMethod classifies the install. Detection is best-effort and
// conservative: it prefers refusing (homebrew/container) over a surprising
// self-replacement when signals are present.
func detectInstallMethod(version, path string) InstallMethod {
	if !isReleaseVersion(version) {
		return InstallDev
	}
	if isHomebrewPath(path) {
		return InstallHomebrew
	}
	if inContainer() {
		return InstallContainer
	}
	return InstallDirect
}

// isHomebrewPath reports whether the executable lives under a Homebrew prefix.
func isHomebrewPath(path string) bool {
	if strings.Contains(path, "/Cellar/") {
		return true
	}
	prefixes := []string{"/opt/homebrew/", "/usr/local/Cellar/", "/home/linuxbrew/.linuxbrew/"}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// inContainer reports whether the process appears to run inside a container.
func inContainer() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat(dockerEnvPath); err == nil {
		return true
	}
	if data, err := os.ReadFile(cgroupPath); err == nil {
		c := string(data)
		if strings.Contains(c, "docker") || strings.Contains(c, "containerd") || strings.Contains(c, "kubepods") {
			return true
		}
	}
	return false
}

// isWritable reports whether the current user can replace the binary by writing
// a temp file in its directory (which is what atomic replacement requires).
func isWritable(path string) bool {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".infrahub-update-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
