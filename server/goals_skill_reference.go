package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const maxGoalsSkillReferenceBytes = 256 << 10
const goalsSkillReferenceError = "Skill 参考文档读取失败：仅支持本轮已授权 Skill 的 SKILL.md 或 refs/references 内受限 Markdown、文本文件。"

type goalsReferenceRoot struct {
	logical string
	real    string
	info    os.FileInfo
	root    *os.Root
}

// newGoalsSkillReferenceTool captures open directory roots for this assembled
// run. No directory comes from model arguments; cleanup releases every handle.
func newGoalsSkillReferenceTool(skillDirs map[string]string) (actool.CoreTool, func(), error) {
	roots := make([]goalsReferenceRoot, 0, len(skillDirs))
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			for _, root := range roots {
				_ = root.root.Close()
			}
		})
	}
	keys := make([]string, 0, len(skillDirs))
	for name := range skillDirs {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		logical := filepath.Clean(skillDirs[name])
		if !filepath.IsAbs(logical) {
			cleanup()
			return nil, func() {}, errors.New("已授权 Skill 的参考目录不可用")
		}
		real, err := filepath.EvalSymlinks(logical)
		if err != nil {
			cleanup()
			return nil, func() {}, errors.New("已授权 Skill 的参考目录不可用")
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			cleanup()
			return nil, func() {}, errors.New("已授权 Skill 的参考目录不可用")
		}
		root, err := os.OpenRoot(real)
		if err != nil {
			cleanup()
			return nil, func() {}, errors.New("已授权 Skill 的参考目录不可用")
		}
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = root.Close()
			cleanup()
			return nil, func() {}, errors.New("已授权 Skill 的参考目录在装配时发生变化")
		}
		roots = append(roots, goalsReferenceRoot{logical: logical, real: real, info: info, root: root})
	}
	if len(roots) == 0 {
		return nil, cleanup, nil
	}
	tool := actool.Build(actool.Spec{
		Name:        "read_skill_reference",
		Description: "只读本轮已授权 Skill 的知识文档，不是通用 Read。Skill 索引要求 Read 时，用本工具读取展开后的绝对路径；仅支持 SKILL.md 和 refs/references 下 .md/.txt，最多 256 KiB，不读配置、脚本或任意文件，不执行内容、不解锁 MCP。",
		Schema:      objSchema(map[string]any{"path": strParam("Skill 索引中已展开的绝对知识文档路径")}, "path"),
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var request struct {
				Path string `json:"path"`
			}
			if len(input) == 0 || len(input) > 16<<10 {
				return actool.Errorf(goalsSkillReferenceError), nil
			}
			decoder := json.NewDecoder(bytes.NewReader(input))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return actool.Errorf(goalsSkillReferenceError), nil
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				return actool.Errorf(goalsSkillReferenceError), nil
			}
			text, err := readGoalsSkillReference(ctx, roots, request.Path)
			if err != nil {
				return actool.Errorf(goalsSkillReferenceError), nil
			}
			return actool.Text(text), nil
		},
	})
	return tool, cleanup, nil
}

func goalsDocumentRelative(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) == 1 {
		return relative, strings.EqualFold(parts[0], "SKILL.md")
	}
	extension := strings.ToLower(filepath.Ext(relative))
	return relative, (strings.EqualFold(parts[0], "refs") || strings.EqualFold(parts[0], "references")) && (extension == ".md" || extension == ".txt")
}

func readGoalsSkillReference(ctx context.Context, roots []goalsReferenceRoot, path string) (string, error) {
	deny := errors.New("restricted Skill reference")
	if ctx.Err() != nil || path == "" || len(path) > 8192 || !filepath.IsAbs(path) || !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", deny
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", deny
		}
	}
	path = filepath.Clean(path)
	lexicalAllowed := false
	for _, root := range roots {
		if _, allowed := goalsDocumentRelative(root.logical, path); allowed {
			current, err := os.Stat(root.logical)
			if err == nil && os.SameFile(root.info, current) {
				lexicalAllowed = true
				break
			}
		}
	}
	if !lexicalAllowed {
		return "", deny
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", deny
	}
	for _, root := range roots {
		relative, allowed := goalsDocumentRelative(root.real, real)
		if !allowed {
			continue
		}
		expected, err := root.root.Stat(relative)
		if err != nil || !expected.Mode().IsRegular() || expected.Size() > maxGoalsSkillReferenceBytes {
			return "", deny
		}
		file, err := openGoalsReferenceFile(root.root, relative)
		if err != nil {
			return "", deny
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxGoalsSkillReferenceBytes || !os.SameFile(expected, info) {
			return "", deny
		}
		// Recheck the canonical name and opened identity before reading. This
		// rejects an inside-root .md link switched to .env/config/script content.
		currentReal, err := filepath.EvalSymlinks(filepath.Join(root.real, relative))
		if err != nil {
			return "", deny
		}
		if _, allowed := goalsDocumentRelative(root.real, currentReal); !allowed {
			return "", deny
		}
		currentInfo, err := os.Stat(currentReal)
		if err != nil || !os.SameFile(info, currentInfo) {
			return "", deny
		}
		stop := context.AfterFunc(ctx, func() { _ = file.Close() })
		defer stop()
		data, err := io.ReadAll(io.LimitReader(file, maxGoalsSkillReferenceBytes+1))
		if err != nil || ctx.Err() != nil || len(data) > maxGoalsSkillReferenceBytes || !utf8.Valid(data) || strings.IndexFunc(string(data), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) >= 0 {
			return "", deny
		}
		return string(data), nil
	}
	return "", deny
}
