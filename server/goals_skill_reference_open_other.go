//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package server

import "os"

func openRootFileForRead(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
