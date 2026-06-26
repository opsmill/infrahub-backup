package updater

import (
	"context"
	"fmt"
)

// resolveRelease returns the release to act on: a specific tag when
// opts.TargetVersion is set (pin/downgrade), otherwise the latest stable.
func resolveRelease(ctx context.Context, opts Options) (*Release, error) {
	if opts.TargetVersion != "" {
		if !isReleaseVersion(opts.TargetVersion) {
			return nil, fmt.Errorf("invalid target version %q: expected a semver tag like v1.7.2", opts.TargetVersion)
		}
		return ReleaseByTag(ctx, opts.TargetVersion)
	}
	return LatestRelease(ctx)
}

// Check reports whether an update is available without modifying anything on
// disk.
func Check(ctx context.Context, opts Options) (*UpdateResult, error) {
	inst, err := inspectInstall(opts.CurrentVersion)
	if err != nil {
		return nil, err
	}

	if inst.InstallMethod == InstallDev {
		return &UpdateResult{
			FromVersion:   opts.CurrentVersion,
			Action:        ActionRefused,
			RefusedReason: "development build: version cannot be compared against releases",
		}, nil
	}

	rel, err := resolveRelease(ctx, opts)
	if err != nil {
		return nil, err
	}

	res := &UpdateResult{
		FromVersion: inst.Version,
		ToVersion:   rel.TagName,
		DetailsURL:  rel.HTMLURL,
	}
	if opts.TargetVersion == "" && !isNewer(inst.Version, rel.TagName) {
		res.Action = ActionAlreadyCurrent
		return res, nil
	}
	if opts.TargetVersion != "" && rel.TagName == inst.Version {
		res.Action = ActionAlreadyCurrent
		return res, nil
	}
	res.Action = ActionAvailable
	return res, nil
}

// Update downloads, verifies, and atomically installs the target release in
// place of the running binary. proceed is invoked once the from/to versions are
// known and must return true for the replacement to occur; returning false
// cancels the update without writing. A nil proceed means "always proceed".
func Update(ctx context.Context, opts Options, proceed func(from, to string) (bool, error)) (*UpdateResult, error) {
	inst, err := inspectInstall(opts.CurrentVersion)
	if err != nil {
		return nil, err
	}

	// Eligibility gates — refuse (exit 1) with an actionable alternative.
	if reason := refusalReason(inst); reason != "" {
		return &UpdateResult{
			FromVersion:   inst.Version,
			Action:        ActionRefused,
			RefusedReason: reason,
		}, nil
	}

	rel, err := resolveRelease(ctx, opts)
	if err != nil {
		return nil, err
	}

	res := &UpdateResult{
		FromVersion: inst.Version,
		ToVersion:   rel.TagName,
		DetailsURL:  rel.HTMLURL,
	}

	// No-op when already current (unless a different specific version was pinned).
	if opts.TargetVersion == "" && !isNewer(inst.Version, rel.TagName) {
		res.Action = ActionAlreadyCurrent
		return res, nil
	}
	if opts.TargetVersion != "" && rel.TagName == inst.Version {
		res.Action = ActionAlreadyCurrent
		return res, nil
	}

	plat := currentPlatform(opts.BinaryName)
	asset, err := plat.selectAsset(rel)
	if err != nil {
		return nil, err
	}

	if proceed != nil {
		ok, perr := proceed(inst.Version, rel.TagName)
		if perr != nil {
			return nil, perr
		}
		if !ok {
			res.Action = ActionRefused
			res.RefusedReason = "update cancelled"
			return res, nil
		}
	}

	sumsAsset, err := checksumsAsset(rel)
	if err != nil {
		return nil, err
	}
	sums, err := fetchChecksums(ctx, sumsAsset)
	if err != nil {
		return nil, err
	}
	digest, err := sums.digestFor(asset.Name)
	if err != nil {
		return nil, err
	}

	if err := applyUpdate(ctx, asset, digest, inst.Path); err != nil {
		return nil, err
	}

	res.Action = ActionUpdated
	return res, nil
}

// refusalReason returns a non-empty, user-facing reason when self-update is not
// permitted for this install, or "" when it is allowed.
func refusalReason(inst InstalledBinary) string {
	switch inst.InstallMethod {
	case InstallDev:
		return "self-update is only available for released builds (this is a development build)"
	case InstallHomebrew:
		return "this binary is managed by Homebrew — run `brew upgrade infrahub-backup`"
	case InstallContainer:
		return "running inside a container — pull a newer image tag instead of self-updating"
	}
	if !inst.Writable {
		return fmt.Sprintf("cannot update %s: permission denied; re-run with elevated privileges", inst.Path)
	}
	return ""
}
