package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withAPIBase(t *testing.T, url string) {
	t.Helper()
	old := apiBaseURL
	apiBaseURL = url
	t.Cleanup(func() { apiBaseURL = old })
}

func TestLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/opsmill/infrahub-backup/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.8.0","html_url":"http://example/v1.8.0","assets":[{"name":"infrahub-backup-linux-amd64","browser_download_url":"http://x/a"}]}`))
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v1.8.0" || len(rel.Assets) != 1 {
		t.Errorf("unexpected release: %+v", rel)
	}
}

func TestReleaseByTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/opsmill/infrahub-backup/releases/tags/v1.7.2" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.7.2"}`))
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	rel, err := ReleaseByTag(context.Background(), "v1.7.2")
	if err != nil {
		t.Fatalf("ReleaseByTag: %v", err)
	}
	if rel.TagName != "v1.7.2" {
		t.Errorf("got %q", rel.TagName)
	}

	if _, err := ReleaseByTag(context.Background(), "v0.0.0"); err == nil {
		t.Error("expected error for unknown tag")
	}
}

func TestTokenHeaderSent(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "secret-token")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"tag_name":"v1.8.0"}`))
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	if _, err := LatestRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
}

func TestRateLimitReported(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	_, err := LatestRelease(context.Background())
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	if !contains(err.Error(), "rate limit") {
		t.Errorf("error %q should mention rate limit", err.Error())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
