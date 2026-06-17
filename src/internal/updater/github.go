package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// apiBaseURL is the GitHub REST API base. It is a variable so tests can point it
// at an httptest.Server.
var apiBaseURL = "https://api.github.com"

// httpTimeout bounds API calls so an unreachable source fails fast rather than
// hanging (supports the <60s / <10s success criteria).
const httpTimeout = 30 * time.Second

// httpClient is the client used for API and asset requests.
var httpClient = &http.Client{Timeout: httpTimeout}

// authToken returns a GitHub token from the environment, if any. A token is
// never required for public releases; it only lifts the unauthenticated rate
// limit for CI and shared-IP/SSH use.
func authToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}

// doJSON performs a GET against the GitHub API and decodes the JSON body into v.
func doJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := authToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach GitHub release source: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkRateLimit(resp); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release not found (HTTP 404)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}
	return nil
}

// checkRateLimit returns a clear, actionable error when the request was
// rejected due to the unauthenticated rate limit.
func checkRateLimit(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if authToken() == "" {
			return fmt.Errorf("GitHub API rate limit exceeded; set GITHUB_TOKEN (or GH_TOKEN) to raise the limit")
		}
		return fmt.Errorf("GitHub API rate limit exceeded even with a token; try again later")
	}
	return fmt.Errorf("GitHub API request forbidden (HTTP %d)", resp.StatusCode)
}

// LatestRelease returns the latest stable (non-draft, non-prerelease) release.
func LatestRelease(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBaseURL, repoOwner, repoName)
	var rel Release
	if err := doJSON(ctx, url, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// ReleaseByTag returns the release for a specific tag (e.g. "v1.7.2"), allowing
// pins and downgrades to prerelease tags.
func ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", apiBaseURL, repoOwner, repoName, tag)
	var rel Release
	if err := doJSON(ctx, url, &rel); err != nil {
		return nil, fmt.Errorf("look up release %s: %w", tag, err)
	}
	return &rel, nil
}

// downloadAsset fetches an asset body. The caller must close the returned reader.
func downloadAsset(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	if tok := authToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
