//go:build !unix

package app

import "io/fs"

// fileOwner reports no ownership on platforms without syscall.Stat_t (Windows).
//
// The co-located runner is always Linux, so nothing real depends on this: it
// exists so that package app cross-compiles for every target in `make build-all`,
// and so that a developer running the worker directly on such a platform gets a
// no-op rather than a build failure. preserveOwnership treats "not reported" as
// "nothing to restore".
func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
