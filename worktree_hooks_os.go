//go:build !js

package git

import (
	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
)

func nativeOSFilesystemRootImpl(filesystem billy.Filesystem) (string, bool) {
	switch filesystem := filesystem.(type) {
	case *osfs.BoundOS:
		return filesystem.Root(), true
	case *osfs.RootOS:
		return filesystem.Root(), true
	default:
		return "", false
	}
}
