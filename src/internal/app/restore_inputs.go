package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/sirupsen/logrus"
)

// A restore reads from a pool of archives and needs somewhere to put the intermediate files
// it makes along the way: an object downloaded from a bucket, the plaintext of an encrypted
// archive. This file is the one convention all of them follow — never write into the pool
// under a name the pool could recognize.
//
// Writing to the archive's own name is what made two separate archives disappear: an
// `s3://…` restore truncated the host's local copy of the same name and then deleted it,
// and decrypting `…tar.gz.enc` overwrote and then deleted a `…tar.gz` of the same timestamp
// sitting beside it. Both reported success. A reserved temporary name removes the whole
// class: it cannot be an existing archive's name, it cannot match backupNamePattern — so a
// file a crash left behind is invisible to retention and to `--latest`, neither selectable
// nor prunable as if it were a backup — and it is created rather than merely named, so two
// concurrent restores cannot land on the same path.
const (
	// s3URIRestoreTempPattern is what a positional `s3://bucket/key` restore downloads
	// through, into the backup directory. `create --s3-upload --s3-keep-local` puts the
	// same archive name in both the bucket and that directory, so the collision this
	// avoids is the expected state of that flow rather than a rare accident.
	s3URIRestoreTempPattern = "restore-s3-uri-*.download"

	// decryptRestoreTempPattern is what an encrypted archive is decrypted through, in the
	// directory the archive itself lives in — the same filesystem, so a plaintext the size
	// of the backup does not land somewhere with no room for it.
	decryptRestoreTempPattern = "restore-decrypted-*.archive"
)

// reserveRestoreTempPath creates an empty file in dir under pattern and returns its path,
// closing the handle again: every caller writes the file through something that opens the
// path itself. The name stays reserved on disk, which is what makes it safe against a
// concurrent restore sharing the directory.
func reserveRestoreTempPath(dir, pattern string) (string, error) {
	// A restore may well run on a host that has never taken a backup, so the directory is
	// created rather than assumed.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("failed to create a temporary file in %s: %w", dir, err)
	}

	if err := file.Close(); err != nil {
		os.Remove(file.Name())

		return "", fmt.Errorf("failed to prepare the temporary file %s: %w", file.Name(), err)
	}

	return file.Name(), nil
}

// removeRestoreTempPath discards a reserved path once the restore is done with it.
//
// A cleanup failure is reported rather than returned: the restore's own outcome is what the
// caller has to see, and a leftover cannot pollute the pool either way — the name is not a
// backup archive name. An already-removed path is not a failure, which is what lets callers
// defer this alongside an earlier explicit removal.
func removeRestoreTempPath(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logrus.Warnf("Failed to remove the temporary restore file %s: %v", path, err)
	}
}
