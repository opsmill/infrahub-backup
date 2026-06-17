package updater

import "golang.org/x/mod/semver"

// isReleaseVersion reports whether v is a valid release version string. semver
// requires a leading "v" (e.g. "v1.7.3"), which matches the project's tags.
func isReleaseVersion(v string) bool {
	return semver.IsValid(v)
}

// isNewer reports whether target is a strictly newer version than current.
// Both must be valid semver; callers validate eligibility beforehand.
func isNewer(current, target string) bool {
	return semver.Compare(current, target) < 0
}
