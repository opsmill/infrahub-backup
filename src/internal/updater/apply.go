package updater

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/minio/selfupdate"
)

// applyUpdate downloads the platform asset and atomically replaces the binary
// at targetPath, verifying the expected SHA-256 first. On any failure the
// previous binary is left in place (selfupdate performs the swap atomically
// with rollback).
func applyUpdate(ctx context.Context, asset Asset, hexDigest, targetPath string) error {
	checksum, err := hex.DecodeString(hexDigest)
	if err != nil {
		return fmt.Errorf("invalid checksum %q: %w", hexDigest, err)
	}

	body, err := downloadAsset(ctx, asset.BrowserDownloadURL)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	opts := selfupdate.Options{Checksum: checksum, TargetPath: targetPath}
	if err := selfupdate.Apply(body, opts); err != nil {
		// selfupdate attempts an automatic rollback; surface that state.
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			return fmt.Errorf("update failed and rollback also failed: %w (rollback: %v)", err, rerr)
		}
		return fmt.Errorf("apply update: %w", err)
	}
	return nil
}
