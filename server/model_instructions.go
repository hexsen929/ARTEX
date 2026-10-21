package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/google/uuid"
)

const maxModelInstructionsSize = 1 << 20

var errInvalidModelInstructions = errors.New("模型指令文件无效")

type modelInstructionsFileInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

type codexConfigInspection struct {
	ConfigPath                 string `json:"config_path"`
	ModelInstructionsFile      string `json:"model_instructions_file"`
	ModelInstructionsBytes     int    `json:"model_instructions_bytes"`
	DeveloperInstructionsBytes int    `json:"developer_instructions_bytes"`
	HasModelInstructions       bool   `json:"has_model_instructions"`
}

// Return only the paths and status of the selected model file. Codex's config
// can contain unrelated provider credentials and must never be returned whole.
func (s *Server) inspectCodexConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeModelInstructionsJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	loaded, err := config.LoadCodexInstructions(body.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info := codexConfigInspection{ConfigPath: loaded.ConfigPath, ModelInstructionsFile: loaded.ModelInstructionsPath, ModelInstructionsBytes: len(loaded.BaseInstructions), DeveloperInstructionsBytes: len(loaded.DeveloperInstructions), HasModelInstructions: loaded.ModelInstructionsPath != ""}
	writeJSON(w, http.StatusOK, info)
}

func validateCodexInstructionsSource(path string) (string, error) {
	loaded, err := config.LoadCodexInstructions(path)
	if err != nil {
		return loaded.ConfigPath, fmt.Errorf("%w: %s", errInvalidModelInstructions, err)
	}
	return loaded.ConfigPath, nil
}

type modelInstructionsStore struct{ dir string }

func instructionFilename(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

func (s *Server) instructionStore() modelInstructionsStore {
	dir := s.modelInstructionsDir
	if dir == "" {
		dir = filepath.Join(config.BaseDir(), "model-instructions")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return modelInstructionsStore{dir: dir}
}

func validateModelInstructionsName(name string) error {
	ext := strings.ToLower(filepath.Ext(name))
	if name == "" || len(name) > 200 || !utf8.ValidString(name) || strings.TrimSpace(name) != name ||
		strings.HasPrefix(name, ".") || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) ||
		(ext != ".md" && ext != ".txt") {
		return fmt.Errorf("%w: 文件名必须是 .md 或 .txt 文件名，不能包含路径", errInvalidModelInstructions)
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return fmt.Errorf("%w: 文件名不能包含控制字符", errInvalidModelInstructions)
		}
	}
	return nil
}

// Preserve text exactly, except the optional UTF-8 BOM at the beginning.
func normalizeModelInstructions(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if len(data) > maxModelInstructionsSize {
		return nil, fmt.Errorf("%w: 正文不能超过 1 MiB", errInvalidModelInstructions)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: 正文必须为 UTF-8 文本", errInvalidModelInstructions)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("%w: 正文不能为空", errInvalidModelInstructions)
	}
	return data, nil
}

func (st modelInstructionsStore) open(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(st.dir, 0o700); err != nil {
			return nil, err
		}
	}
	fi, err := os.Lstat(st.dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: 管理目录不能是符号链接或普通文件", errInvalidModelInstructions)
	}
	// Root keeps all operations anchored in this directory, including during renames.
	root, err := os.OpenRoot(st.dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(fi, opened) {
		root.Close()
		return nil, fmt.Errorf("%w: 管理目录在打开前发生变化，请重试", errInvalidModelInstructions)
	}
	return root, nil
}

func modelInstructionsRegular(root *os.Root, name string) (os.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return fi, fmt.Errorf("%w: 不允许符号链接、目录或特殊文件", errInvalidModelInstructions)
	}
	return fi, nil
}

func (st modelInstructionsStore) read(name string) (modelInstructionsFileInfo, []byte, error) {
	info := modelInstructionsFileInfo{Name: name, Path: filepath.Join(st.dir, name)}
	if err := validateModelInstructionsName(name); err != nil {
		return info, nil, err
	}
	root, err := st.open(false)
	if err != nil {
		return info, nil, err
	}
	defer root.Close()
	fi, err := modelInstructionsRegular(root, name)
	if fi != nil {
		info.Exists, info.Size = true, fi.Size()
	}
	if err != nil {
		return info, nil, err
	}
	if fi.Size() > maxModelInstructionsSize+3 {
		return info, nil, fmt.Errorf("%w: 正文不能超过 1 MiB", errInvalidModelInstructions)
	}
	f, err := root.Open(name)
	if err != nil {
		return info, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return info, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(fi, opened) {
		return info, nil, fmt.Errorf("%w: 文件在读取前发生变化，请重试", errInvalidModelInstructions)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxModelInstructionsSize+4))
	if err != nil {
		return info, nil, err
	}
	data, err = normalizeModelInstructions(data)
	return info, data, err
}

func (st modelInstructionsStore) list() ([]modelInstructionsFileInfo, error) {
	root, err := st.open(false)
	if errors.Is(err, os.ErrNotExist) {
		return []modelInstructionsFileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	root.Close()
	if err != nil {
		return nil, err
	}
	files := make([]modelInstructionsFileInfo, 0, len(entries))
	for _, entry := range entries {
		if err := validateModelInstructionsName(entry.Name()); err != nil {
			continue
		}
		info, _, err := st.read(entry.Name())
		if err != nil {
			info.Error = err.Error()
		}
		files = append(files, info)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func (st modelInstructionsStore) write(name string, data []byte, create bool) (modelInstructionsFileInfo, error) {
	if err := validateModelInstructionsName(name); err != nil {
		return modelInstructionsFileInfo{}, err
	}
	data, err := normalizeModelInstructions(data)
	if err != nil {
		return modelInstructionsFileInfo{}, err
	}
	root, err := st.open(true)
	if err != nil {
		return modelInstructionsFileInfo{}, err
	}
	defer root.Close()
	if create {
		if _, err := root.Lstat(name); err == nil {
			return modelInstructionsFileInfo{}, os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return modelInstructionsFileInfo{}, err
		}
	} else if _, err := modelInstructionsRegular(root, name); err != nil {
		return modelInstructionsFileInfo{}, err
	}
	temporary := ".instruction-" + uuid.NewString()
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return modelInstructionsFileInfo{}, err
	}
	defer root.Remove(temporary)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return modelInstructionsFileInfo{}, writeErr
	}
	if closeErr != nil {
		return modelInstructionsFileInfo{}, closeErr
	}
	if create {
		// An exclusive hard link publishes the complete file without overwriting
		// another create that raced with this request.
		err = root.Link(temporary, name)
	} else {
		if _, err = modelInstructionsRegular(root, name); err == nil {
			err = root.Rename(temporary, name)
		}
	}
	if err != nil {
		return modelInstructionsFileInfo{}, err
	}
	return modelInstructionsFileInfo{Name: name, Path: filepath.Join(st.dir, name), Size: int64(len(data)), Exists: true}, nil
}

func (st modelInstructionsStore) remove(name string) error {
	if err := validateModelInstructionsName(name); err != nil {
		return err
	}
	root, err := st.open(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := modelInstructionsRegular(root, name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return root.Remove(name)
}

func (s *Server) modelInstructionsPath(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if err := validateModelInstructionsName(name); err != nil {
		return "", err
	}
	info, _, err := s.instructionStore().read(name)
	return info.Path, err
}

// Loading retains a nonempty path when a configured file disappeared or became
// invalid, so provider construction reports the problem instead of silently
// replacing it with default instructions or another provider.
func (s *Server) runtimeModelInstructionsPath(p *db.LLMProfile) string {
	if p.CodexConfigFile != "" || p.ModelInstructionsFile == "" || !codexModelInstructionsEnabled(p.Format, p.UserAgent) {
		return ""
	}
	if err := validateModelInstructionsName(p.ModelInstructionsFile); err != nil {
		// A directory cannot be read as instructions and never escapes the store.
		return s.instructionStore().dir
	}
	return filepath.Join(s.instructionStore().dir, p.ModelInstructionsFile)
}

// Resolve the instruction source before constructing the provider.
// ConfigError deliberately remains attached to the chosen profile: constructing
// an error provider preserves binding instead of silently using the global LLM.
func (s *Server) applyModelInstructionsConfig(cfg *agent.Config, name, codexPath string) {
	cfg.ModelInstructionsFile, cfg.CodexConfigFile = "", ""
	cfg.ConfigError = nil
	if !codexModelInstructionsEnabled(cfg.Provider(), cfg.UserAgent) {
		return
	}
	if codexPath != "" && name != "" {
		cfg.ConfigError = errors.New("Codex 配置指令与手动指令文件不能同时启用")
		return
	}
	if codexPath != "" {
		cfg.CodexConfigFile = codexPath
		loaded, err := config.LoadCodexInstructions(codexPath)
		if loaded.ConfigPath != "" {
			cfg.CodexConfigFile = loaded.ConfigPath
		}
		if err != nil {
			cfg.ConfigError = err
			return
		}
		return
	}
	if name == "" {
		return
	}
	path, err := s.modelInstructionsPath(name)
	cfg.ModelInstructionsFile = path
	if err != nil {
		cfg.ConfigError = err
	}
}

func codexModelInstructionsEnabled(format, userAgent string) bool {
	return format == "openai-responses" && strings.EqualFold(strings.TrimSpace(userAgent), "codex")
}

func (s *Server) validateProfileModelInstructions(p *db.LLMProfile) error {
	seen := make(map[string]bool, len(p.ModelInstructionsFiles))
	for _, name := range p.ModelInstructionsFiles {
		if err := validateModelInstructionsName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("%w: 文件名重复: %s", errInvalidModelInstructions, name)
		}
		seen[name] = true
	}
	if !codexModelInstructionsEnabled(p.Format, p.UserAgent) {
		p.ModelInstructionsFile = ""
		p.CodexConfigFile = ""
		return nil
	}
	if p.CodexConfigFile != "" {
		if p.ModelInstructionsFile != "" {
			return fmt.Errorf("%w: Codex 配置指令与手动指令文件不能同时启用", errInvalidModelInstructions)
		}
		path, err := validateCodexInstructionsSource(p.CodexConfigFile)
		if err == nil {
			p.CodexConfigFile = path
		}
		return err
	}
	if p.ModelInstructionsFile == "" {
		return nil
	}
	if !seen[p.ModelInstructionsFile] {
		return fmt.Errorf("%w: 已启用文件必须在本配置的文件列表中", errInvalidModelInstructions)
	}
	_, err := s.modelInstructionsPath(p.ModelInstructionsFile)
	return err
}

func decodeModelInstructionsJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 6*maxModelInstructionsSize+1024))
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("正文必须为 UTF-8 文本")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("请求必须包含单个 JSON 对象")
	}
	return nil
}

func writeModelInstructionsError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errInvalidModelInstructions):
		status = http.StatusBadRequest
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, os.ErrExist):
		status = http.StatusConflict
	}
	writeErr(w, status, err.Error())
}

func (s *Server) listModelInstructions(w http.ResponseWriter, r *http.Request) {
	st := s.instructionStore()
	files, err := st.list()
	if err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"directory": st.dir, "files": files})
}

func (s *Server) createModelInstructions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := decodeModelInstructionsJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.modelInstructionsMu.Lock()
	defer s.modelInstructionsMu.Unlock()
	info, err := s.instructionStore().write(body.Name, []byte(body.Content), true)
	if err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"file": info})
}

func (s *Server) readModelInstructions(w http.ResponseWriter, r *http.Request) {
	info, content, err := s.instructionStore().read(r.PathValue("name"))
	if err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": info, "content": string(content)})
}

func (s *Server) updateModelInstructions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeModelInstructionsJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.modelInstructionsMu.Lock()
	info, err := s.instructionStore().write(r.PathValue("name"), []byte(body.Content), false)
	s.modelInstructionsMu.Unlock()
	if err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	if s.m != nil && s.m.pg != nil {
		s.invalidateProfileAgents()
		s.reapplyActiveProfile()
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": info})
}

func enabledModelInstructionsProfile(profiles []*db.LLMProfile, name string) *db.LLMProfile {
	for _, p := range profiles {
		if p.ModelInstructionsFile == name {
			return p
		}
	}
	return nil
}

func (s *Server) deleteModelInstructions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateModelInstructionsName(name); err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	s.modelInstructionsMu.Lock()
	defer s.modelInstructionsMu.Unlock()
	profiles, err := pg.ListProfiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p := enabledModelInstructionsProfile(profiles, name); p != nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("配置“%s”正在启用此文件，请先停用并保存该配置后再删除", p.Name))
		return
	}
	if err := s.instructionStore().remove(name); err != nil {
		writeModelInstructionsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}
