package updater

import (
	"fmt"
	"runtime"
)

// currentPlatform builds the PlatformTarget for the running binary, computing
// the expected release asset name as "<binary>-<os>-<arch>[.exe]" to match what
// `make build-all` produces.
func currentPlatform(binaryName string) PlatformTarget {
	return platformFor(binaryName, runtime.GOOS, runtime.GOARCH)
}

// platformFor is the testable core of currentPlatform.
func platformFor(binaryName, goos, goarch string) PlatformTarget {
	asset := fmt.Sprintf("%s-%s-%s", binaryName, goos, goarch)
	if goos == "windows" {
		asset += ".exe"
	}
	return PlatformTarget{
		BinaryName: binaryName,
		OS:         goos,
		Arch:       goarch,
		AssetName:  asset,
	}
}

// selectAsset returns the release asset matching the platform target, or an
// error naming the unsupported platform.
func (t PlatformTarget) selectAsset(rel *Release) (Asset, error) {
	for _, a := range rel.Assets {
		if a.Name == t.AssetName {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("no release artifact %q for %s/%s in %s", t.AssetName, t.OS, t.Arch, rel.TagName)
}

// checksumsAsset returns the SHA256SUMS asset from a release, if present.
func checksumsAsset(rel *Release) (Asset, error) {
	for _, a := range rel.Assets {
		if a.Name == "SHA256SUMS" {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no SHA256SUMS asset", rel.TagName)
}
