//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package server

import (
	"os"
	"syscall"
)

// A file replaced by a FIFO between stat and open must not block the reader.
// Nonblocking has no effect on ordinary documents; the opened type is rechecked.
func openGoalsReferenceFile(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
