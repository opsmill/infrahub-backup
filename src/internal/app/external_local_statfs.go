//go:build linux || darwin

package app

import "syscall"

// localFreeSpace is the space an unprivileged writer can still use on the
// filesystem holding path: available blocks, not free blocks, because the
// reserve for root is not this process's to spend.
func localFreeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}

	return uint64(stat.Bavail) * uint64(stat.Bsize), nil //nolint:unconvert // Bsize is uint32 on darwin and int64 on linux
}
