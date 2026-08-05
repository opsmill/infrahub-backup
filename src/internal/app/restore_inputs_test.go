package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// localArchiveBody is what the host's own copy of an archive holds, so "the local copy is
// still there, byte for byte" is an assertion about content and not only about a name.
const localArchiveBody = "the operator's own local copy"

// fakeS3ObjectDownloader is an s3Downloader whose download is scripted, recording the key
// and the local path it was asked for.
type fakeS3ObjectDownloader struct {
	body string
	err  error

	keys       []string
	localPaths []string
}

func (f *fakeS3ObjectDownloader) Download(_ context.Context, s3Key, localPath string) error {
	f.keys = append(f.keys, s3Key)
	f.localPaths = append(f.localPaths, localPath)

	if f.err != nil {
		// S3Client.Download removes the file it created before reporting a failure, so the
		// fake leaves the same state behind for the cleanup to be judged against.
		os.Remove(localPath)

		return f.err
	}

	return os.WriteFile(localPath, []byte(f.body), 0o600)
}

var _ s3Downloader = (*fakeS3ObjectDownloader)(nil)

// TestDownloadRestoreObjectUsesACollisionProofTempPath pins the fix for a positional
// `s3://bucket/key` restore: the download lands on a reserved temporary name, so a local
// archive that shares the object's name — what `create --s3-upload --s3-keep-local` leaves
// on every host it runs on — is neither truncated by the download nor deleted with it when
// the restore finishes.
func TestDownloadRestoreObjectUsesACollisionProofTempPath(t *testing.T) {
	const objectBody = "the archive that lives in the bucket"

	dir := t.TempDir()
	archive := backupNameAt(retentionNow)
	localCopy := filepath.Join(dir, archive)
	if err := os.WriteFile(localCopy, []byte(localArchiveBody), 0o600); err != nil {
		t.Fatalf("writing the local copy = %v, want nil", err)
	}

	client := &fakeS3ObjectDownloader{body: objectBody}
	key := "prod/" + archive

	downloaded, err := downloadRestoreObject(context.Background(), client, dir, key)
	if err != nil {
		t.Fatalf("downloadRestoreObject() = %v, want nil", err)
	}

	// Where it landed: in the backup directory, under a name the pool cannot recognize.
	if got := filepath.Dir(downloaded); got != dir {
		t.Errorf("download directory = %q, want the backup directory %q", got, dir)
	}
	name := filepath.Base(downloaded)
	if name == archive {
		t.Fatalf("the object was downloaded onto the archive's own name %q", archive)
	}
	if backupNamePattern.MatchString(name) {
		t.Errorf("download %q matches backupNamePattern: a failed cleanup would pollute the pool", name)
	}
	if !strings.HasPrefix(name, "restore-s3-uri-") || !strings.HasSuffix(name, ".download") {
		t.Errorf("download = %q, want the %q shape", name, s3URIRestoreTempPattern)
	}

	// The object is addressed by the key it was asked for, and its bytes are what the
	// restore will read.
	if want := []string{key}; !slices.Equal(client.keys, want) {
		t.Errorf("downloaded keys = %v, want %v", client.keys, want)
	}
	if want := []string{downloaded}; !slices.Equal(client.localPaths, want) {
		t.Errorf("download paths = %v, want %v", client.localPaths, want)
	}
	body, err := os.ReadFile(downloaded)
	if err != nil {
		t.Fatalf("reading the download = %v, want nil", err)
	}
	if string(body) != objectBody {
		t.Errorf("download = %q, want the object %q", body, objectBody)
	}

	// The local archive is untouched, and removing the download — what the restore does on
	// its way out — leaves it that way.
	removeRestoreTempPath(downloaded)

	survivor, err := os.ReadFile(localCopy)
	if err != nil {
		t.Fatalf("reading the local copy back = %v, want it untouched", err)
	}
	if string(survivor) != localArchiveBody {
		t.Errorf("local copy = %q, want it byte-identical %q", survivor, localArchiveBody)
	}
	if got, want := dirNames(t, dir), []string{archive}; !slices.Equal(got, want) {
		t.Errorf("backup directory = %v, want only the local copy %v", got, want)
	}
}

// TestDownloadRestoreObjectCleansUpAFailedDownload covers the exit nobody watches: a
// download that fails reports why and leaves no half-downloaded archive occupying the backup
// directory, and the local copy of the same name is still there either way.
func TestDownloadRestoreObjectCleansUpAFailedDownload(t *testing.T) {
	downloadFailure := errors.New("connection reset by peer")

	dir := t.TempDir()
	archive := backupNameAt(retentionNow)
	localCopy := filepath.Join(dir, archive)
	if err := os.WriteFile(localCopy, []byte(localArchiveBody), 0o600); err != nil {
		t.Fatalf("writing the local copy = %v, want nil", err)
	}

	client := &fakeS3ObjectDownloader{err: downloadFailure}

	path, err := downloadRestoreObject(context.Background(), client, dir, "prod/"+archive)

	if err == nil {
		t.Fatalf("downloadRestoreObject() = %q, nil, want an error", path)
	}
	if !errors.Is(err, downloadFailure) {
		t.Errorf("error = %q, want it to wrap %v", err, downloadFailure)
	}
	if path != "" {
		t.Errorf("path = %q, want no path alongside an error", path)
	}
	if got, want := dirNames(t, dir), []string{archive}; !slices.Equal(got, want) {
		t.Errorf("backup directory = %v, want only the local copy %v", got, want)
	}
	survivor, err := os.ReadFile(localCopy)
	if err != nil {
		t.Fatalf("reading the local copy back = %v, want it untouched", err)
	}
	if string(survivor) != localArchiveBody {
		t.Errorf("local copy = %q, want it byte-identical %q", survivor, localArchiveBody)
	}
}

// writeEncryptedArchive writes a real encrypted archive at path and returns the private key
// file that opens it. The plaintext is a valid gzip tarball holding an empty backup
// directory, so a restore gets as far as rejecting it for missing metadata — past the
// decryption, and before anything about a deployment is touched.
func writeEncryptedArchive(t *testing.T, path string) string {
	t.Helper()

	keyDir := t.TempDir()
	privPEM, pubB64, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() = %v, want nil", err)
	}
	keyPath := filepath.Join(keyDir, "backup.key")
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		t.Fatalf("writing the private key = %v, want nil", err)
	}
	pubKey, err := LoadPublicKeyFromBase64(pubB64)
	if err != nil {
		t.Fatalf("LoadPublicKeyFromBase64() = %v, want nil", err)
	}

	sourceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceDir, "backup"), 0o755); err != nil {
		t.Fatalf("preparing the archive contents = %v, want nil", err)
	}
	plain := filepath.Join(keyDir, "plain.tar.gz")
	if err := createTarball(plain, sourceDir, "backup/"); err != nil {
		t.Fatalf("createTarball() = %v, want nil", err)
	}

	if err := EncryptFile(plain, path, pubKey); err != nil {
		t.Fatalf("EncryptFile() = %v, want nil", err)
	}

	return keyPath
}

// TestRestoreBackupDecryptsWithoutConsumingASameTimestampArchive pins the fix for the
// decrypt path: restoring `…tar.gz.enc` must not write to `…tar.gz`, because that is a valid
// archive name in the very pool the restore selects from. Before the fix the plaintext
// overwrote such an archive and the restore then deleted it — two archives in the directory,
// one afterwards, and a run that reported success. The `--latest` tiebreak prefers the .enc
// member of a same-timestamp pair, so this was reachable without naming the encrypted
// archive at all.
//
// The restore itself is expected to fail: the archive carries no metadata, which is the
// first thing checked after the decryption and before any container is touched. That the
// decryption happened at all is asserted from the plaintext the run had to read.
func TestRestoreBackupDecryptsWithoutConsumingASameTimestampArchive(t *testing.T) {
	dir := t.TempDir()
	plainName := backupNameAt(retentionNow)
	plainArchive := filepath.Join(dir, plainName)
	encryptedName := plainName + encryptedArchiveSuffix
	encryptedArchive := filepath.Join(dir, encryptedName)

	keyPath := writeEncryptedArchive(t, encryptedArchive)
	encryptedBefore, err := os.ReadFile(encryptedArchive)
	if err != nil {
		t.Fatalf("reading the encrypted archive = %v, want nil", err)
	}

	// The same-timestamp plain archive that shared the pool. Its contents are not a real
	// archive: nothing may read it, so nothing may notice.
	if err := os.WriteFile(plainArchive, []byte(localArchiveBody), 0o600); err != nil {
		t.Fatalf("writing the plain archive = %v, want nil", err)
	}

	iops := NewInfrahubOps()
	iops.backend = &bareBackend{name: "docker"}
	iops.config.BackupDir = dir

	err = iops.RestoreBackup(encryptedArchive, false, false, 0, keyPath, false, false)

	// The decryption ran and produced a readable archive: the run got as far as reading it
	// and finding no metadata inside.
	if err == nil {
		t.Fatalf("RestoreBackup() = nil, want the archive rejected for missing metadata")
	}
	if !strings.Contains(err.Error(), "missing metadata") {
		t.Fatalf("RestoreBackup() = %q, want it to have decrypted and then rejected the archive", err)
	}

	// Both archives are still there, and both are byte-identical.
	survivor, err := os.ReadFile(plainArchive)
	if err != nil {
		t.Fatalf("reading the plain archive back = %v, want it untouched", err)
	}
	if string(survivor) != localArchiveBody {
		t.Errorf("plain archive = %q, want it byte-identical %q", survivor, localArchiveBody)
	}
	encryptedAfter, err := os.ReadFile(encryptedArchive)
	if err != nil {
		t.Fatalf("reading the encrypted archive back = %v, want it untouched", err)
	}
	if string(encryptedAfter) != string(encryptedBefore) {
		t.Error("the encrypted archive was modified by the restore")
	}

	// And the plaintext is not left behind for a later --latest to rank.
	if got, want := dirNames(t, dir), []string{plainName, encryptedName}; !slices.Equal(got, want) {
		t.Errorf("backup directory = %v, want exactly the two archives %v", got, want)
	}
}
