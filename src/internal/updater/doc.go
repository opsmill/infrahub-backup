// Package updater implements the self-update capability for the infrahub-backup
// and infrahub-taskmanager binaries.
//
// The pipeline is: discover the relevant GitHub Release of
// opsmill/infrahub-backup, select the artifact matching the running OS/arch,
// verify its SHA-256 checksum against the release's SHA256SUMS asset, and
// atomically replace the running binary (with rollback on failure) using
// github.com/minio/selfupdate.
//
// Updates are only applied to standalone-binary installations. Builds installed
// via a package manager (Homebrew), running inside a container image, or built
// from source without a release version are detected and refused with guidance
// toward the correct upgrade path.
package updater
