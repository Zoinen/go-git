//go:build js

package git

import "github.com/go-git/go-billy/v6"

// osfs on js/wasm is memory-backed. It must never be treated as a host path
// for a process runner.
func nativeOSFilesystemRootImpl(billy.Filesystem) (string, bool) {
	return "", false
}
