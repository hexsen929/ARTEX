package server

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const managedSavePrefix = ".artex-save-"

var errManagedFileType = errors.New("unexpected file type")

func openManagedRegularFile(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	before, err := root.Stat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errManagedFileType
	}
	file, err := openRootFileForRead(root, rel)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = errManagedFileType
		}
		return nil, nil, err
	}
	return file, info, nil
}

// Commit a complete copy in the destination directory. The final rename
// replaces the directory entry without modifying the previous file's inode.
func replaceManagedFile(root *os.Root, rel string, data io.Reader, mode os.FileMode) error {
	parent, err := root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(rel)
	switch info, err := parent.Lstat(name); {
	case err == nil:
		if !info.Mode().IsRegular() {
			return errManagedFileType
		}
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	tempName := managedSavePrefix + rand.Text()
	file, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer parent.Remove(tempName)
	_, copyErr := io.Copy(file, data)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	return parent.Rename(tempName, name)
}

func openRealChildDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errManagedFileType
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		child.Close()
		if err == nil {
			err = errManagedFileType
		}
		return nil, err
	}
	return child, nil
}
