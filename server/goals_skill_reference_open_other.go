//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package server

import "os"

func openGoalsReferenceFile(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
