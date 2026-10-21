package server

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// zip-upload safety caps (defeat zip bombs / runaway archives).
const (
	maxSkillZipBytes   = 20 << 20  // 20MB compressed request body
	maxSkillTotalBytes = 100 << 20 // 100MB total uncompressed
	maxSkillFileBytes  = 20 << 20  // 20MB per extracted file
	maxSkillEntries    = 4000      // max files in the archive
)

// fsUploadSkill installs a skill from an uploaded .zip. The archive must contain a
// SKILL.md (at the root or under a single top-level dir); the skill name is taken
// from that file's `name:` frontmatter (falling back to the top dir / zip name).
// Zip-slip is defeated by validating every entry path with skillRelPath, and a set
// of size/count caps guard against zip bombs. POST ?overwrite=true replaces an
// existing skill of the same name.
func (s *Server) fsUploadSkill(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSkillZipBytes)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少上传文件(表单字段 file)或超出大小限制")
		return
	}
	defer file.Close()
	buf, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	zr, err := newSkillZipReader(buf)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// entries carry UTF-8-decoded names (GBK 包也能读) and exclude archiver junk.
	entriesAll := skillZipEntries(zr)
	if err := checkSkillZipMethods(entriesAll); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	// locate the shallowest SKILL.md → its directory is the skill root inside the zip.
	var skillMD *skillZipEntry
	for i := range entriesAll {
		e := &entriesAll[i]
		if path.Base(e.name) != "SKILL.md" {
			continue
		}
		if skillMD == nil || strings.Count(e.name, "/") < strings.Count(skillMD.name, "/") {
			skillMD = e
		}
	}
	if skillMD == nil {
		writeErr(w, 400, "压缩包内未找到 SKILL.md")
		return
	}
	root := path.Dir(skillMD.name) // "." when SKILL.md is at the zip root
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}

	// derive + validate the skill name from the SKILL.md frontmatter.
	md, err := readZipEntry(skillMD.f)
	if err != nil {
		writeErr(w, 400, "读取 SKILL.md 失败："+err.Error())
		return
	}
	name := skillNameFromFrontmatter(md)
	if name == "" && prefix != "" {
		name = path.Base(strings.TrimSuffix(prefix, "/"))
	}
	if name == "" {
		base := path.Base(filepath.ToSlash(hdr.Filename))
		name = strings.TrimSuffix(base, path.Ext(base))
	}
	if !validSkillName(name) {
		writeErr(w, 400, "skill 名称无效（取自 SKILL.md 的 name 字段）："+name+
			"（≤64 字符，字母开头，只能用小写字母/数字/连字符或中文等非 ASCII 字母，不能有空格、点、路径分隔符）")
		return
	}

	skillPath := filepath.Join(s.skillDir, name)
	overwrite := r.URL.Query().Get("overwrite") == "true"
	if _, err := os.Stat(skillPath); err == nil && !overwrite {
		writeErr(w, 409, "skill 已存在："+name+"（如需覆盖请确认后重试）")
		return
	}

	// extract into a temp dir first, then atomically swap in — a bad entry aborts
	// the whole upload without leaving a half-written skill.
	_ = os.MkdirAll(s.skillDir, 0o755)
	tmp, err := os.MkdirTemp(s.skillDir, ".upload-*")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(tmp) // no-op after a successful rename

	var total int64
	entries := 0
	for _, e := range entriesAll {
		f := e.f
		// only files under the skill root; skip anything outside it.
		if prefix != "" && !strings.HasPrefix(e.name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(e.name, prefix)
		if rel == "" {
			continue
		}
		clean, msg := skillRelPath(rel)
		if msg != "" {
			writeErr(w, 400, "压缩包含非法路径 "+e.name+"："+msg)
			return
		}
		if entries++; entries > maxSkillEntries {
			writeErr(w, 400, "压缩包文件过多")
			return
		}
		if f.UncompressedSize64 > maxSkillFileBytes {
			writeErr(w, 400, "文件过大："+rel)
			return
		}
		dst := filepath.Join(tmp, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		rc, err := f.Open()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			writeErr(w, 500, err.Error())
			return
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxSkillFileBytes+1))
		out.Close()
		rc.Close()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		total += n
		if total > maxSkillTotalBytes {
			writeErr(w, 400, "压缩包解压后过大")
			return
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
		writeErr(w, 400, "解压后缺少 SKILL.md")
		return
	}

	if overwrite {
		_ = os.RemoveAll(skillPath)
	}
	if err := os.Rename(tmp, skillPath); err != nil {
		writeErr(w, 500, "安装失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"name": name, "files": entries})
}

// readZipEntry reads a single entry's bytes (capped). Takes the *zip.File rather than
// a name because zr.Open rejects names that aren't valid UTF-8 (GBK-named archives).
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSkillFileBytes))
}
