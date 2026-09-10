package app

import (
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3KeyClient builds a client carrying configuration only. The key filtering and
// key construction under test are pure functions of the prefix, so no endpoint,
// credentials, or network access are involved.
func s3KeyClient(bucket, prefix string) *S3Client {
	return &S3Client{config: &S3Config{Bucket: bucket, Prefix: prefix}}
}

func TestS3ClientListPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "no prefix lists the bucket root", prefix: "", want: ""},
		{name: "prefix gains a trailing slash", prefix: "backups/prod", want: "backups/prod/"},
		{name: "trailing slash is preserved once", prefix: "backups/prod/", want: "backups/prod/"},
		{name: "single segment", prefix: "backups", want: "backups/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s3KeyClient("bucket", tc.prefix).listPrefix(); got != tc.want {
				t.Errorf("listPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestS3ClientBackupRefForKey covers FR-006 at the S3 location: only objects
// written where uploads write them, whose base name is a backup archive with a
// parseable timestamp, become retention candidates.
func TestS3ClientBackupRefForKey(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		key      string
		want     bool
		wantName string
	}{
		// Recognized candidates under a configured prefix.
		{
			name:     "plain archive under the prefix",
			prefix:   "backups/prod",
			key:      "backups/prod/infrahub_backup_20260804_120000.tar.gz",
			want:     true,
			wantName: "infrahub_backup_20260804_120000.tar.gz",
		},
		{
			name:     "encrypted archive under the prefix",
			prefix:   "backups/prod",
			key:      "backups/prod/infrahub_backup_20260801_090000.tar.gz.enc",
			want:     true,
			wantName: "infrahub_backup_20260801_090000.tar.gz.enc",
		},
		{
			name:     "prefix configured with a trailing slash matches the same keys",
			prefix:   "backups/prod/",
			key:      "backups/prod/infrahub_backup_20260804_120000.tar.gz",
			want:     true,
			wantName: "infrahub_backup_20260804_120000.tar.gz",
		},
		{
			name:     "archive at the bucket root when no prefix is configured",
			prefix:   "",
			key:      "infrahub_backup_20260804_120000.tar.gz",
			want:     true,
			wantName: "infrahub_backup_20260804_120000.tar.gz",
		},

		// Objects sharing the prefix that are not backups.
		{name: "unrelated object", prefix: "backups/prod", key: "backups/prod/notes.txt"},
		{name: "backup-like without timestamp", prefix: "backups/prod", key: "backups/prod/infrahub_backup_garbage.tar.gz"},
		{name: "unparseable timestamp", prefix: "backups/prod", key: "backups/prod/infrahub_backup_20261301_120000.tar.gz"},
		{name: "checksum sidecar", prefix: "backups/prod", key: "backups/prod/infrahub_backup_20260804_120000.tar.gz.sha256"},
		{name: "other tarball", prefix: "backups/prod", key: "backups/prod/somebackup.tar.gz"},

		// Keys outside the exact prefix level are invisible even if their base name
		// is a valid archive name.
		{name: "nested below the prefix", prefix: "backups/prod", key: "backups/prod/nested/infrahub_backup_20260804_120000.tar.gz"},
		{name: "sibling prefix", prefix: "backups/prod", key: "backups/prod-old/infrahub_backup_20260804_120000.tar.gz"},
		{name: "bucket root while a prefix is configured", prefix: "backups/prod", key: "infrahub_backup_20260804_120000.tar.gz"},
		{name: "parent of the prefix", prefix: "backups/prod", key: "backups/infrahub_backup_20260804_120000.tar.gz"},
		{name: "prefixed key while no prefix is configured", prefix: "", key: "backups/infrahub_backup_20260804_120000.tar.gz"},

		// Listing artifacts.
		{name: "common prefix entry", prefix: "backups/prod", key: "backups/prod/"},
		{name: "empty key", prefix: "backups/prod", key: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := s3KeyClient("bucket", tc.prefix)
			ref, ok := client.backupRefForKey(tc.key)
			if ok != tc.want {
				t.Fatalf("backupRefForKey(%q) with prefix %q: ok = %v, want %v", tc.key, tc.prefix, ok, tc.want)
			}
			if !tc.want {
				if ref != (backupRef{}) {
					t.Errorf("backupRefForKey(%q) returned %+v alongside ok=false, want the zero ref", tc.key, ref)
				}
				return
			}
			if ref.Name != tc.wantName {
				t.Errorf("ref.Name = %q, want the base name %q", ref.Name, tc.wantName)
			}
			// The deletion key must be reconstructible from the base name alone.
			if got := client.buildS3Key(ref.Name); got != tc.key {
				t.Errorf("buildS3Key(%q) = %q, want the listed key %q", ref.Name, got, tc.key)
			}
		})
	}
}

// TestS3ClientBuildS3Key pins the delete-key construction that s3Location.Delete
// relies on to turn a base-name ref back into the object it listed.
func TestS3ClientBuildS3Key(t *testing.T) {
	const name = "infrahub_backup_20260804_120000.tar.gz"

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "no prefix", prefix: "", want: name},
		{name: "prefix without trailing slash", prefix: "backups/prod", want: "backups/prod/" + name},
		{name: "prefix with trailing slash", prefix: "backups/prod/", want: "backups/prod/" + name},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s3KeyClient("bucket", tc.prefix).buildS3Key(name); got != tc.want {
				t.Errorf("buildS3Key(%q) = %q, want %q", name, got, tc.want)
			}
		})
	}
}

// TestCredentialProbeIsBounded is T141. With no credentials in the
// environment, every S3 request falls through to the IAM provider's reach for
// the link-local metadata service, and on a host that has none that address is
// blackholed rather than refused. Unbounded, one object read against an
// unreachable endpoint spent its whole s3StatTimeout on it — 125 seconds for a
// HEAD that failed immediately, which is the runtime swing this test's own
// package used to show.
//
// The bound is asserted at the two places it can be lost: the dialer's timeout,
// and the provider actually being given a client that carries it. Neither is
// visible in a run's output, so a regression in either reads as nothing but
// slowness.
func TestCredentialProbeIsBounded(t *testing.T) {
	dialer := credentialProbeDialer()
	if dialer.Timeout <= 0 {
		t.Error("credentialProbeDialer() has no timeout: the metadata probe waits out the transport's default, which is what made a failed object read take two minutes")
	}
	if dialer.Timeout > 5*time.Second {
		t.Errorf("credentialProbeDialer().Timeout = %v, want a short bound: a link-local service either answers at once or is not there", dialer.Timeout)
	}

	providers, err := credentialProviders()
	if err != nil {
		t.Fatalf("credentialProviders() = %v, want nil", err)
	}

	iam := 0
	for _, provider := range providers {
		metadata, ok := provider.(*credentials.IAM)
		if !ok {
			continue
		}
		iam++
		if metadata.Client == nil {
			t.Error("the IAM provider was given no client, so it probes the metadata service through http.DefaultClient and the bound above applies to nothing")

			continue
		}
		if _, ok := metadata.Client.Transport.(*http.Transport); !ok {
			t.Errorf("the IAM provider's client carries transport %T, want the bounded *http.Transport", metadata.Client.Transport)
		}
	}
	if iam != 1 {
		t.Errorf("credentialProviders() holds %d IAM providers, want exactly 1: the metadata probe is the one that reaches the network", iam)
	}
}
