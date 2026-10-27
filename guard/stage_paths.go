package guard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrStageProtectedPath   = errors.New("文件属于平台配置或私密数据，停止发布")
	ErrStageSourceChanged   = errors.New("文件来源在检查期间发生变化，停止发布")
	ErrStagePathUnavailable = errors.New("文件来源路径检查失败，停止发布")
)

// StagePathPolicy is the immutable path boundary shared by every managed-file
// entry point. Callers supply the real configuration/key files and private data
// directories, not entire BaseDir/dataDir roots that also contain public tools.
// A dedicated key directory may be protected as a directory; the default
// KeyDir==BaseDir must instead enumerate its secret files.
type StagePathPolicy struct {
	files   []string
	dirs    []string
	invalid bool
}

func NewStagePathPolicy(protectedFiles, protectedDirs []string) *StagePathPolicy {
	p := &StagePathPolicy{}
	for _, entry := range protectedFiles {
		if entry == "" {
			continue
		}
		path, err := filepath.Abs(entry)
		if err != nil {
			p.invalid = true
			continue
		}
		p.files = append(p.files, filepath.Clean(path))
		if resolved, err := filepath.EvalSymlinks(path); err == nil && filepath.Clean(resolved) != filepath.Clean(path) {
			p.files = append(p.files, filepath.Clean(resolved))
		}
	}
	for _, entry := range protectedDirs {
		if entry == "" {
			continue
		}
		path, err := filepath.Abs(entry)
		if err != nil {
			p.invalid = true
			continue
		}
		p.dirs = append(p.dirs, filepath.Clean(path))
		if resolved, err := filepath.EvalSymlinks(path); err == nil && filepath.Clean(resolved) != filepath.Clean(path) {
			p.dirs = append(p.dirs, filepath.Clean(resolved))
		}
	}
	return p
}

func stagePathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// stageProtectedCanonical handles roots that will be created after startup.
// Resolution is repeated on each check so newly-created symlink aliases of a
// protected directory do not remain authorized from a stale startup snapshot.
func stageProtectedCanonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func (p *StagePathPolicy) lexicallyProtected(path string) bool {
	for _, file := range p.files {
		if path == file {
			return true
		}
	}
	for _, dir := range p.dirs {
		if stagePathWithin(path, dir) {
			return true
		}
	}
	return false
}

// Check is suitable for FilePrecheck before Open. Missing/non-regular sources
// fail closed, so an accidental FIFO or directory cannot become a download.
// All public errors intentionally omit both the path and filesystem details.
func (p *StagePathPolicy) Check(path string) error {
	_, _, err := p.inspect(path)
	return err
}

// CheckOpenFile runs after Open and before any reading/copying. The descriptor
// must still identify the inspected canonical file, defeating a symlink change
// between precheck and opening. Known secret-file hard links are also denied.
func (p *StagePathPolicy) CheckOpenFile(path string, file *os.File) error {
	if file == nil {
		return ErrStagePathUnavailable
	}
	_, info, err := p.inspect(path)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return ErrStagePathUnavailable
	}
	if !os.SameFile(opened, info) {
		return ErrStageSourceChanged
	}
	for _, protected := range p.files {
		if secret, err := os.Stat(protected); err == nil && os.SameFile(opened, secret) {
			return ErrStageProtectedPath
		}
	}
	return nil
}

func (p *StagePathPolicy) inspect(path string) (string, os.FileInfo, error) {
	if p == nil || p.invalid || path == "" {
		return "", nil, ErrStagePathUnavailable
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, ErrStagePathUnavailable
	}
	abs = filepath.Clean(abs)
	if p.lexicallyProtected(abs) {
		return "", nil, ErrStageProtectedPath
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, ErrStagePathUnavailable
	}
	canonical = filepath.Clean(canonical)
	if p.lexicallyProtected(canonical) {
		return "", nil, ErrStageProtectedPath
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, ErrStagePathUnavailable
	}
	for _, protected := range p.files {
		if canonical == stageProtectedCanonical(protected) {
			return "", nil, ErrStageProtectedPath
		}
		if secret, err := os.Stat(protected); err == nil && os.SameFile(info, secret) {
			return "", nil, ErrStageProtectedPath
		}
	}
	for _, protected := range p.dirs {
		if stagePathWithin(canonical, stageProtectedCanonical(protected)) {
			return "", nil, ErrStageProtectedPath
		}
		// Canonical paths on case-insensitive filesystems can retain user-supplied
		// casing. Compare directory identities as well, not only string prefixes.
		if protectedInfo, err := os.Stat(protected); err == nil && protectedInfo.IsDir() {
			for ancestor := filepath.Dir(canonical); ; ancestor = filepath.Dir(ancestor) {
				if current, err := os.Stat(ancestor); err == nil && os.SameFile(current, protectedInfo) {
					return "", nil, ErrStageProtectedPath
				}
				if parent := filepath.Dir(ancestor); parent == ancestor {
					break
				}
			}
		}
	}
	return canonical, info, nil
}
