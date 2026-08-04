package app

import "testing"

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
