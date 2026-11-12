package server

import (
	"context"
	"fmt"
	"github.com/Autumn-27/artex/kb"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) saveOperatorPOC(ctx context.Context, e kb.Entry) (kb.Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return e, false, err
	}
	if e.Content != "" {
		root := operatorKnowledgeDir()
		if root == "" {
			root = filepath.Join(s.dataDir, "poc-library")
		}
		pocs := filepath.Join(root, "pocs")
		if err := os.MkdirAll(pocs, 0700); err != nil {
			return e, false, err
		}
		if fi, err := os.Lstat(pocs); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return e, false, fmt.Errorf("POC root must be a real directory")
		}
		dir := filepath.Join(pocs, e.Code)
		if fi, err := os.Lstat(dir); err == nil && (!fi.IsDir() || fi.Mode()&os.ModeSymlink != 0) {
			return e, false, fmt.Errorf("POC entry path is not a real directory")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return e, false, err
		}
		name := e.Filename
		if name == "" {
			ext := map[string]string{"python": "py", "bash": "sh", "go": "go", "java": "java", "http": "http", "js": "js", "php": "php"}[e.Language]
			if ext == "" {
				ext = "md"
			}
			name = "poc." + ext
		}
		if name != filepath.Base(name) || strings.ContainsAny(name, "\\/\x00") || strings.HasPrefix(name, ".") || len(name) > 150 {
			return e, false, fmt.Errorf("invalid POC filename")
		}
		f, err := os.CreateTemp(dir, ".poc-write-")
		if err != nil {
			return e, false, err
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		if err = f.Chmod(0600); err == nil {
			_, err = f.WriteString(e.Content)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return e, false, err
		}
		dst := filepath.Join(dir, name)
		if fi, er := os.Lstat(dst); er == nil && (!fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0) {
			return e, false, fmt.Errorf("POC destination must be a regular file")
		}
		if err = os.Rename(tmp, dst); err != nil {
			return e, false, err
		}
		e.Path = dst
	}
	return s.m.pg.KBPut(ctx, e)
}
