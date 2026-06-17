package updater

// repoOwner and repoName identify the GitHub repository whose releases drive
// self-update. They match the canonical distribution repo.
const (
	repoOwner = "opsmill"
	repoName  = "infrahub-backup"
)

// InstallMethod classifies how the running binary was installed, which
// determines whether self-update is permitted.
type InstallMethod string

const (
	// InstallDirect is a standalone binary download — eligible for self-update.
	InstallDirect InstallMethod = "direct"
	// InstallHomebrew is a Homebrew-managed install — use `brew upgrade`.
	InstallHomebrew InstallMethod = "homebrew"
	// InstallContainer is a binary baked into a container image — pull a newer tag.
	InstallContainer InstallMethod = "container"
	// InstallDev is an unversioned/development build — not a released artifact.
	InstallDev InstallMethod = "dev"
)

// Release is a published version of the tool as returned by the GitHub Releases
// API. Only the fields the updater needs are decoded.
type Release struct {
	TagName    string  `json:"tag_name"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Asset is a single downloadable file attached to a Release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// PlatformTarget describes which release asset the running binary requires.
type PlatformTarget struct {
	BinaryName string
	OS         string
	Arch       string
	AssetName  string
}

// InstalledBinary describes the currently running executable and how it was
// installed.
type InstalledBinary struct {
	Version       string
	Path          string
	Writable      bool
	InstallMethod InstallMethod
}

// Checksums maps an asset filename to its lowercase hex SHA-256 digest, parsed
// from a release's SHA256SUMS asset.
type Checksums struct {
	ByFilename map[string]string
}

// Action describes the outcome of a Check or Update run.
type Action string

const (
	// ActionUpdated means the binary was replaced with a newer version.
	ActionUpdated Action = "updated"
	// ActionAlreadyCurrent means the installed version is current or newer.
	ActionAlreadyCurrent Action = "already-current"
	// ActionAvailable means an update exists (check mode; nothing written).
	ActionAvailable Action = "available"
	// ActionRefused means self-update is not permitted for this install.
	ActionRefused Action = "refused"
)

// UpdateResult is returned to the command layer to render output.
type UpdateResult struct {
	FromVersion   string
	ToVersion     string
	Action        Action
	RefusedReason string
	DetailsURL    string
}

// Options controls a Check or Update invocation.
type Options struct {
	// BinaryName is the base name of the invoked binary (e.g. "infrahub-backup").
	BinaryName string
	// CurrentVersion is the running binary's reported version.
	CurrentVersion string
	// TargetVersion, when non-empty, pins to a specific release tag (pin/downgrade).
	TargetVersion string
}
