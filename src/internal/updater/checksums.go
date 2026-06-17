package updater

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// fetchChecksums downloads and parses a SHA256SUMS asset into a Checksums map.
func fetchChecksums(ctx context.Context, asset Asset) (*Checksums, error) {
	body, err := downloadAsset(ctx, asset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return parseChecksums(body)
}

// parseChecksums reads standard `sha256sum` output ("<hex>␠␠<filename>", one per
// line) into a Checksums map keyed by filename.
func parseChecksums(r io.Reader) (*Checksums, error) {
	out := &Checksums{ByFilename: map[string]string{}}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed SHA256SUMS line: %q", line)
		}
		// The filename may be prefixed with "*" in binary mode; strip it.
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		out.ByFilename[name] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if len(out.ByFilename) == 0 {
		return nil, fmt.Errorf("SHA256SUMS is empty")
	}
	return out, nil
}

// digestFor returns the expected hex digest for an asset name.
func (c *Checksums) digestFor(name string) (string, error) {
	d, ok := c.ByFilename[name]
	if !ok {
		return "", fmt.Errorf("no checksum entry for %q in SHA256SUMS", name)
	}
	return d, nil
}
