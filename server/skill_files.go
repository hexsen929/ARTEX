package server

import (
	"path"

	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// validSkillName checks the agentskills.io name constraints, widened so a skill can
// also be named in Chinese (or any other script): ASCII must stay lowercase
// alphanumeric + hyphens, while non-ASCII letters/digits are accepted as-is.
// 1-64 runes, must start with a letter, no leading/trailing/consecutive hyphens.
// The name doubles as a directory name under skillDir, so nothing that could carry a
// path (separators, dots, spaces, control characters) is allowed through.
func validSkillName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	rs := []rune(name)
	if len(rs) > 64 {
		return false
	}
	isLetter := func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r > unicode.MaxASCII && unicode.IsLetter(r))
	}
	if !isLetter(rs[0]) || rs[len(rs)-1] == '-' {
		return false
	}
	for _, r := range rs {
		switch {
		case isLetter(r), r >= '0' && r <= '9', r == '-':
		case r > unicode.MaxASCII && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return !strings.Contains(name, "--")
}

// skillPathBlocked lists the ASCII characters a skill-relative path may not contain.
// '\' is a Windows separator; '%' keeps double-URL-encoding tricks from surviving
// (Go's net/http URL-decodes PathValue once — %2F→/ %2e→. — so an attacker sending
// %252e%252e arrives here as the literal "%2e%2e" and is rejected on the '%'); '#'
// and '?' would truncate the path when it travels back through a URL; the rest are
// reserved on Windows filesystems.
const skillPathBlocked = `\%#?*:"<>|`

// skillPathRune reports whether r may appear in a client-supplied skill path.
// It is a blacklist over Unicode rather than an ASCII whitelist so that 中文 (and any
// other script) file names work, while everything that makes path validation hard is
// still refused: control/format characters, look-alike whitespace, separators.
func skillPathRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r == utf8.RuneError:
		return false // NUL & control characters, invalid UTF-8
	case strings.ContainsRune(skillPathBlocked, r):
		return false
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
		return false // zero-width joiners, bidi overrides (RLO 文件名伪装), private use
	case r != ' ' && unicode.IsSpace(r):
		return false // NBSP / 全角空格 之类：看着是空格，其实不是
	}
	return true
}

// maxSkillPathLen caps a relative path so a pathological name can't reach the syscall.
const maxSkillPathLen = 512

// skillRelPath validates a relative file path supplied by the client.
// Returns the cleaned path and an empty error string on success.
// Validation order matters: character check first (before Clean) so encoding tricks
// can't survive normalization.
func skillRelPath(file string) (string, string) {
	// 1. Character check — before normalization. Defeats null bytes, backslash,
	//    '%', control characters and unicode look-alikes; CJK names pass through.
	if file == "" || len(file) > maxSkillPathLen {
		return "", "invalid path: empty or too long"
	}
	if !utf8.ValidString(file) {
		return "", "invalid path: not valid UTF-8"
	}
	for _, r := range file {
		if !skillPathRune(r) {
			return "", "invalid path: illegal character " + strconv.QuoteRune(r)
		}
	}
	// 2. Reject ".." explicitly. With the whitelist above, encoding bypass is already
	//    impossible, but we keep this check so the intent is obvious to reviewers.
	if strings.Contains(file, "..") {
		return "", "invalid path: '..' not allowed"
	}
	// 3. Reject leading slash and empty segments (double slash).
	//    Leading slash would survive filepath.Clean as an absolute path.
	if strings.HasPrefix(file, "/") || strings.Contains(file, "//") {
		return "", "invalid path: must be relative with no empty segments"
	}
	// 4. Normalize as a URL/API path, not as a host filesystem path. API paths
	//    always use '/', including when ARTEX runs on Windows. Filesystem calls
	//    below accept this canonical form through filepath.Join.
	clean := path.Clean(file)
	if clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", "invalid path"
	}
	return filepath.ToSlash(clean), ""
}

// walkSkillFiles returns all files under root (relative to root), sorted,
// including files in subdirectories (scripts/, references/, assets/, etc.).
// walkSkillFiles returns all entries under root relative to root.
// Directories are included with a trailing "/" so the frontend can distinguish
// them from files and render empty folders in the tree.
func walkSkillFiles(root string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		// The file list is an HTTP API contract consumed by a frontend that splits
		// paths on '/'. filepath.Rel uses '\\' on Windows, so normalize before the
		// value leaves the backend.
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, rel+"/")
		} else {
			entries = append(entries, rel)
		}
		return nil
	})
	return entries, err
}

func (s *Server) fsListFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	dirPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill not found")
		return
	}
	files, err := walkSkillFiles(dirPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if files == nil {
		files = []string{}
	}
	writeJSON(w, 200, map[string]any{"files": files})
}

func (s *Server) fsReadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.skillDir, name, file))
	if os.IsNotExist(err) {
		writeErr(w, 404, "file not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"content": string(data), "file": file})
}

func (s *Server) fsWriteFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	skillPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill not found")
		return
	}
	fullPath := filepath.Join(skillPath, file)
	// create parent subdirectory if needed (e.g. scripts/, references/, assets/)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(fullPath, []byte(body.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) fsCreateDir(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir, errMsg := skillRelPath(body.Path)
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	skillPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill not found")
		return
	}
	if err := os.MkdirAll(filepath.Join(skillPath, dir), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"dir": dir})
}

func (s *Server) fsDeletePath(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "invalid skill name")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	fullPath := filepath.Join(s.skillDir, name, file)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		writeErr(w, 404, "not found")
		return
	}
	if err := os.RemoveAll(fullPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": file})
}
