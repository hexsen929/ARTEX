package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const maxSkillIndexBytes = 4 << 20

// SkillEntryInspection describes loading, not execution or dependency health.
// Errors are fixed summaries: YAML/file errors can contain private index text.
type SkillEntryInspection struct {
	Loadable     bool   `json:"loadable"`
	LoadError    string `json:"load_error,omitempty"`
	DeclaredName string `json:"declared_name,omitempty"`
}

type inspectedSkillFrontmatter struct {
	Name          string    `yaml:"name"`
	Description   string    `yaml:"description"`
	License       string    `yaml:"license"`
	Compatibility string    `yaml:"compatibility"`
	WhenToUse     string    `yaml:"whenToUse"`
	WhenToUseUS   string    `yaml:"when_to_use"`
	MCPs          yaml.Node `yaml:"mcps"`
	MCP           yaml.Node `yaml:"mcp"`
}

// inspectSkillEntry uses the same directory-name authorization and frontmatter
// fields as the SDK, while exposing the malformed/missing entries that its
// permissive loader would otherwise skip or degrade silently.
func inspectSkillEntry(root, name string) SkillEntryInspection {
	result := SkillEntryInspection{DeclaredName: name}
	fail := func(message string) SkillEntryInspection {
		result.LoadError = message
		return result
	}
	if strings.TrimSpace(root) == "" || name == "" || name == "." || name == ".." || !utf8.ValidString(name) || filepath.Base(name) != name || filepath.VolumeName(name) != "" || strings.ContainsAny(name, `/\`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fail("Skill 目录名称无效")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fail("Skill 根目录无效")
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fail("Skill 根目录不存在或不可读取")
	}
	if info, err := os.Stat(rootReal); err != nil || !info.IsDir() {
		return fail("Skill 根目录不存在或不是目录")
	}
	entry := filepath.Join(rootAbs, name)
	entryInfo, err := os.Lstat(entry)
	if err != nil {
		return fail("Skill 目录不存在或不可读取")
	}
	// Configuring the root itself as a symlink is supported. Individual symlink
	// directories are not loaded by norma.LoadDir (DirEntry.IsDir is false), so
	// report that limitation rather than calling a skipped entry loadable.
	if entryInfo.Mode()&os.ModeSymlink != 0 {
		return fail("当前 Skill 加载器未装载符号链接目录")
	}
	if !entryInfo.IsDir() {
		return fail("Skill 项不是目录")
	}
	index := filepath.Join(entry, "SKILL.md")
	indexReal, err := filepath.EvalSymlinks(index)
	if err != nil {
		return fail("缺少 SKILL.md 或入口文件不可读取")
	}
	rel, err := filepath.Rel(rootReal, indexReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fail("SKILL.md 指向配置的 Skill 根目录之外")
	}
	info, err := os.Stat(indexReal)
	if err != nil || !info.Mode().IsRegular() {
		return fail("SKILL.md 不是可读取的普通文件")
	}
	f, err := os.Open(indexReal)
	if err != nil {
		return fail("SKILL.md 不可读取")
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = f.Close()
		return fail("SKILL.md 在读取前发生变化，请重试")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSkillIndexBytes+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fail("SKILL.md 读取失败")
	}
	if len(data) > maxSkillIndexBytes {
		return fail("SKILL.md 超出可加载长度限制")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fail("SKILL.md 需要有效的 UTF-8 文本")
	}
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		return fail("请移除 SKILL.md 开头的 UTF-8 BOM")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	body := text
	if strings.HasPrefix(text, "---") {
		lines := strings.Split(text, "\n")
		if strings.TrimSpace(lines[0]) != "---" {
			return fail("Skill frontmatter 起始分隔符格式无效")
		}
		closing := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimRight(lines[i], " \t") == "---" {
				closing = i
				break
			}
		}
		if closing < 0 {
			return fail("Skill frontmatter 缺少结束分隔符")
		}
		var fm inspectedSkillFrontmatter
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &fm); err != nil {
			return fail("Skill frontmatter 不是有效的 YAML 元数据")
		}
		if !skillMCPFieldReadable(&fm.MCPs) || !skillMCPFieldReadable(&fm.MCP) {
			return fail("Skill MCP 依赖声明应为名称或名称列表")
		}
		if fm.Name != "" {
			if fm.Name != strings.TrimSpace(fm.Name) || strings.IndexFunc(fm.Name, unicode.IsControl) >= 0 {
				return fail("Skill 名称包含首尾空白或控制字符")
			}
			result.DeclaredName = fm.Name
		}
		body = strings.Join(lines[closing+1:], "\n")
	}
	if strings.TrimSpace(body) == "" {
		return fail("Skill 正文为空")
	}
	result.Loadable = true
	return result
}

func skillMCPFieldReadable(node *yaml.Node) bool {
	if node.Kind == 0 || node.Tag == "!!null" {
		return true
	}
	var scalar string
	if err := node.Decode(&scalar); err == nil {
		return true
	}
	var names []string
	return node.Decode(&names) == nil
}

// inspectSkillCatalog also detects aliases that would overwrite each other in
// the SDK registry. No MCP is connected and no Skill instruction is executed.
func inspectSkillCatalog(root string) (map[string]SkillEntryInspection, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, errors.New("Skill 根目录读取失败")
	}
	result := make(map[string]SkillEntryInspection)
	byDeclared := map[string][]string{}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		inspection := inspectSkillEntry(root, entry.Name())
		result[entry.Name()] = inspection
		if inspection.Loadable {
			byDeclared[inspection.DeclaredName] = append(byDeclared[inspection.DeclaredName], entry.Name())
		}
	}
	for _, names := range byDeclared {
		if len(names) < 2 {
			continue
		}
		for _, name := range names {
			inspection := result[name]
			inspection.Loadable = false
			inspection.LoadError = "Skill 声明名称与其它目录重复"
			result[name] = inspection
		}
	}
	return result, nil
}
