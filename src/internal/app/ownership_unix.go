//go:build unix

package app

import (
	"io/fs"
	"syscall"
)

// fileOwner reports the uid and gid recorded for info, and whether this platform
// reports them at all.
//
// syscall.Stat_t is a Unix type: it does not exist on Windows. Reading it inline
// in run_connector.go broke `GOOS=windows GOARCH=amd64 go build ./src/...`, which
// is one of the five targets `make build-all` (Makefile:38) iterates, and put a
// platform-specific file into package app — the package this project's CLAUDE.md
// tells contributors to build and test as a whole, and where Linux-only code had
// previously been confined to tools/neo4jwatchdog.
func fileOwner(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
