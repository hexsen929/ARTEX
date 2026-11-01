package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Workspace file manager — browse / view / edit / download / upload / delete the
// shared work dir (s.m.dir), where all agents write their artifacts. Every path is
// confined to the work dir root (traversal via ".." is neutralised). All routes sit
// behind requireAuth (see Handler()).

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// wsResolve maps a user-supplied relative path to an absolute path INSIDE the work
// dir. It returns ok=false if the path would escape the root. filepath.Clean on a
// rooted copy collapses any ".." so nothing can climb above the root.
func (s *Server) wsResolve(rel string) (string, bool) {
	base := filepath.Clean(s.m.dir)
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // e.g. "/a/../../etc" → "/etc" (still rooted at "/")
	abs := filepath.Clean(filepath.Join(base, clean))
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// wsRel renders an absolute path back as a workspace-relative path (forward slashes).
func (s *Server) wsRel(abs string) string {
	base := filepath.Clean(s.m.dir)
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		writeErr(w, 404, "路径不存在")
		return
	}
	if !fi.IsDir() {
		writeErr(w, 400, "不是目录")
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  s.wsRel(filepath.Join(abs, e.Name())),
			Dir:   e.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// 目录在前，各自按名称排序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "entries": out})
}

// GET /api/workspace/read?path=<rel> — inline text for view/edit. Binary or oversize
// files return {binary:true}/{too_large:true} with no content (use download instead).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "是目录，不能作为文件读取")
		return
	}
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "binary": false, "content": string(data)})
}

// POST /api/workspace/write  {path, content} — create/overwrite a text file.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, ok := s.wsResolve(req.Path)
	if !ok || abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "非法路径")
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		writeErr(w, 400, "目标是目录")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(abs, []byte(req.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(abs)})
}

// POST /api/workspace/mkdir  {path}
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, ok := s.wsResolve(req.Path)
	if !ok || abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "非法路径")
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(abs)})
}

// DELETE /api/workspace/delete?path=<rel> — removes a file or a directory tree
// (confined to the work dir; the root itself can't be deleted).
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	if abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "不能删除工作区根目录")
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeErr(w, 404, "路径不存在")
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — stream a file as an attachment.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		writeErr(w, 404, "文件不存在")
		return
	}
	name := filepath.Base(abs)
	// RFC 5987 filename* keeps non-ASCII names intact; plain filename is the fallback.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(name)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeFile(w, r, abs)
}

// GET /api/workspace/preview?path=<rel> — serve an image inline for report
// Markdown. Unlike download, this uses an inline content disposition so a
// report can render a saved browser screenshot with a normal Markdown image URL.
func (s *Server) wsPreview(w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimSpace(r.URL.Query().Get("path"))
	var file *os.File
	var err error
	if _, portable := reportScreenshotRelative(rawPath); portable {
		// A portable report link has no session directory. Resolve its native
		// finding identity, never a newer file in an unrelated session.
		var findingID int64
		if value := r.URL.Query().Get("finding_id"); value != "" {
			findingID, err = strconv.ParseInt(value, 10, 64)
			if err != nil || findingID <= 0 {
				writeErr(w, 400, "非法漏洞编号")
				return
			}
		}
		file, err = s.openPortableReportScreenshot(rawPath, findingID)
	} else {
		abs, ok := s.wsResolve(rawPath)
		if filepath.IsAbs(rawPath) {
			base, candidate := filepath.Clean(s.m.dir), filepath.Clean(rawPath)
			ok = candidate == base || strings.HasPrefix(candidate, base+string(os.PathSeparator))
			if ok {
				abs = candidate
			}
		}
		if !ok {
			writeErr(w, 400, "非法路径")
			return
		}
		workspace, openErr := os.OpenRoot(s.m.dir)
		if openErr != nil {
			writeErr(w, 404, "文件不存在")
			return
		}
		defer workspace.Close()
		relative, relErr := filepath.Rel(filepath.Clean(s.m.dir), abs)
		if relErr != nil {
			writeErr(w, 404, "文件不存在")
			return
		}
		// Serve a confined handle, not a path reopened by ServeFile.
		file, err = workspace.Open(relative)
	}
	if err != nil || file == nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeErr(w, 404, "文件不存在")
		return
	}
	if !isReportImage(rawPath) {
		writeErr(w, 415, "仅支持图片预览")
		return
	}
	if s.m.pg != nil && s.m.pg.GetBool(settingScreenshotTimestamp, false) {
		rel := filepath.ToSlash(rawPath)
		if strings.HasPrefix(rel, "findings/screenshots/") || strings.Contains(rel, "/findings/screenshots/") {
			w.Header().Set("X-Report-Screenshot-Timestamp", reportScreenshotTimestamp(fi.ModTime()))
		}
	}
	name := filepath.Base(rawPath)
	w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilename(name)+"\"")
	http.ServeContent(w, r, name, fi.ModTime(), file)
}

func reportScreenshotTimestamp(t time.Time) string {
	return t.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02 15:04:05")
}

func isReportImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

// reportScreenshotRelative accepts the one portable format documented by the
// reporter. Do not normalize traversal into an apparently valid report link.
func reportScreenshotRelative(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, `\`) || filepath.IsAbs(raw) {
		return "", false
	}
	rel := filepath.FromSlash(raw)
	const prefix = "findings" + string(os.PathSeparator) + "screenshots" + string(os.PathSeparator)
	if filepath.Clean(rel) != rel || !strings.HasPrefix(rel, prefix) || !isReportImage(rel) {
		return "", false
	}
	return rel, true
}

var reportScreenshotNode = regexp.MustCompile(`(?i)^finding-([1-9][0-9]*)-step-[1-9][0-9]*\.(png|jpg|jpeg|webp|gif)$`)

func (s *Server) openPortableReportScreenshot(raw string, findingID int64) (*os.File, error) {
	if s.m.pg == nil {
		return nil, os.ErrNotExist
	}
	var nodeID int64
	if findingID <= 0 {
		// Compatibility for the reporter's original canonical filenames. New
		// detail views send the native finding ID explicitly, so older arbitrary
		// screenshot basenames do not need a global-session fallback either.
		match := reportScreenshotNode.FindStringSubmatch(filepath.Base(raw))
		if len(match) != 3 {
			return nil, os.ErrNotExist
		}
		var err error
		nodeID, err = strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, os.ErrNotExist
		}
		findingID, err = s.m.pg.FindingIDByNodeID(nodeID)
		if err != nil || findingID <= 0 {
			return nil, os.ErrNotExist
		}
	}
	finding, err := s.m.pg.GetFinding(findingID)
	if err != nil || finding == nil || (nodeID > 0 && (finding.NodeID == nil || *finding.NodeID != nodeID)) {
		return nil, os.ErrNotExist
	}
	var taskID, conversationID int64
	if finding.TaskID != nil {
		taskID = *finding.TaskID
	}
	if finding.ReportConversationID != nil {
		// A bare persisted pointer can come from an old archive or an obsolete
		// writer. Only an exact host binding can authorize session fallback.
		resolved, resolveErr := s.m.pg.ResolveFindingReportConversation(finding.ID)
		if resolveErr == nil && resolved != nil && *resolved == *finding.ReportConversationID {
			conversationID = *resolved
		}
	}
	file, _, err := openReportScreenshot(s.m.dir, taskID, conversationID, raw)
	return file, err
}

// openScreenshotDirectoryRoot obtains an independent capability for exactly one
// task/session. Reject alias directories, including aliases to another task
// inside the data root; OpenRoot's escape protection alone cannot distinguish
// those. SameFile also rejects a directory replaced between Lstat and OpenRoot.
func openScreenshotDirectoryRoot(workspaceDir, collection, name string, create bool) (*os.Root, error) {
	workspace, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return nil, err
	}
	defer workspace.Close()
	openChild := func(parent *os.Root, child string) (*os.Root, error) {
		if create {
			if err := parent.Mkdir(child, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		before, err := parent.Lstat(child)
		if err != nil {
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("截图目录不是独立目录")
		}
		root, err := parent.OpenRoot(child)
		if err != nil {
			return nil, err
		}
		after, err := root.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			root.Close()
			return nil, fmt.Errorf("截图目录已变更")
		}
		return root, nil
	}
	parent, err := openChild(workspace, collection)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return openChild(parent, name)
}

func openRegularScreenshot(root *os.Root, rel string) (*os.File, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("截图不是普通文件")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, fmt.Errorf("截图文件已变更")
	}
	return file, nil
}

// openReportScreenshot selects only the finding's task archive or authoritative
// report conversation. Explicit absolute/workspace paths must identify that
// same owner. No glob, mtime selection, or other-session fallback is permitted.
// The returned file is already open through the selected owner's capability.
func openReportScreenshot(workspaceDir string, taskID, conversationID int64, raw string) (*os.File, string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "workspace://")
	value = strings.TrimPrefix(value, "file://")
	if filepath.IsAbs(value) {
		rel, err := filepath.Rel(filepath.Clean(workspaceDir), filepath.Clean(value))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, "", os.ErrNotExist
		}
		value = filepath.ToSlash(rel)
	}
	type candidate struct{ collection, name, rel string }
	var candidates []candidate
	if rel, portable := reportScreenshotRelative(value); portable {
		if taskID > 0 {
			candidates = append(candidates, candidate{"tasks", strconv.FormatInt(taskID, 10), rel})
		}
		if conversationID > 0 {
			candidates = append(candidates, candidate{"sessions", "conv-" + strconv.FormatInt(conversationID, 10), rel})
		}
	} else {
		for _, owner := range []candidate{{"tasks", strconv.FormatInt(taskID, 10), ""}, {"sessions", "conv-" + strconv.FormatInt(conversationID, 10), ""}} {
			if (owner.collection == "tasks" && taskID <= 0) || (owner.collection == "sessions" && conversationID <= 0) {
				continue
			}
			prefix := owner.collection + "/" + owner.name + "/"
			if strings.HasPrefix(value, prefix) {
				if rel, ok := reportScreenshotRelative(strings.TrimPrefix(value, prefix)); ok {
					owner.rel = rel
					candidates = append(candidates, owner)
				}
			}
		}
	}
	for _, candidate := range candidates {
		root, err := openScreenshotDirectoryRoot(workspaceDir, candidate.collection, candidate.name, false)
		if err != nil {
			continue
		}
		file, err := openRegularScreenshot(root, candidate.rel)
		root.Close()
		if err == nil {
			if err = verifyReportSnapshot(file, candidate.rel); err != nil {
				file.Close()
				continue
			}
			return file, filepath.ToSlash(filepath.Join(candidate.collection, candidate.name, candidate.rel)), nil
		}
	}
	return nil, "", os.ErrNotExist
}

const maxReportScreenshotSnapshot = 64 << 20 // bounded per-image capture, not an unbounded file buffer

var reportSnapshotPath = regexp.MustCompile(`^findings/screenshots/\.snapshots/([a-f0-9]{64})/[^/]+$`)

func reportSnapshotDigest(rel string) (string, bool) {
	match := reportSnapshotPath.FindStringSubmatch(filepath.ToSlash(rel))
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func verifyReportSnapshot(file *os.File, rel string) error {
	digest, snapshot := reportSnapshotDigest(rel)
	if !snapshot {
		if strings.HasPrefix(filepath.ToSlash(rel), "findings/screenshots/.snapshots/") {
			return fmt.Errorf("截图快照路径无效")
		}
		return nil // old explicit images remain readable, without claiming a digest
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxReportScreenshotSnapshot+1))
	if err != nil {
		return err
	}
	if size > maxReportScreenshotSnapshot || fmt.Sprintf("%x", hash.Sum(nil)) != digest {
		return fmt.Errorf("截图快照摘要不匹配")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

// prepareFindingReportScreenshots binds each current capture to immutable bytes,
// then rewrites the exact Markdown URL that the atomic report transaction saves.
// A rejected/stale report may leave an unreferenced hash snapshot, but it cannot
// occupy a mutable basename that a later successful report would accidentally use.
func (s *Server) prepareFindingReportScreenshots(taskID, conversationID int64, markdown string) (string, error) {
	if taskID <= 0 {
		return markdown, nil
	}
	var out strings.Builder
	resolved := map[string]string{}
	last := 0
	for _, match := range screenshotMarkdownLink.FindAllStringSubmatchIndex(markdown, -1) {
		out.WriteString(markdown[last:match[0]])
		raw := markdown[match[4]:match[5]]
		rel, portable := reportScreenshotRelative(raw)
		if !portable {
			out.WriteString(markdown[match[0]:match[1]])
			last = match[1]
			continue
		}
		snapshot, cached := resolved[raw]
		if !cached {
			file, _, err := openReportScreenshot(s.m.dir, 0, conversationID, raw)
			if err != nil {
				// Report edits may retain an already committed immutable URL.
				// Only such a digest path may reuse task data instead of a fresh
				// capture; ordinary basenames never prefer a stale task copy.
				if _, immutable := reportSnapshotDigest(rel); immutable {
					file, _, err = openReportScreenshot(s.m.dir, taskID, 0, raw)
				}
			}
			if err != nil {
				out.WriteString(markdown[match[0]:match[1]])
				last = match[1]
				continue
			}
			snapshot, err = s.saveReportScreenshotSnapshot(taskID, rel, file)
			closeErr := file.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return "", err
			}
			resolved[raw] = snapshot
		}
		out.WriteString(markdown[match[0]:match[4]])
		out.WriteString(snapshot)
		out.WriteString(markdown[match[5]:match[1]])
		last = match[1]
	}
	out.WriteString(markdown[last:])
	return out.String(), nil
}

func (s *Server) saveReportScreenshotSnapshot(taskID int64, rel string, in *os.File) (string, error) {
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(in, maxReportScreenshotSnapshot+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxReportScreenshotSnapshot {
		return "", fmt.Errorf("截图超过单张 64 MiB 的快照上限")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	snapshotDir := filepath.Join("findings", "screenshots", ".snapshots", digest)
	snapshot := filepath.Join(snapshotDir, filepath.Base(rel))
	task, err := openScreenshotDirectoryRoot(s.m.dir, "tasks", strconv.FormatInt(taskID, 10), true)
	if err != nil {
		return "", err
	}
	defer task.Close()
	if err := task.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", err
	}
	// Publish only a fully written file. Link is no-replace/atomic, so concurrent
	// captures of the same bytes cannot expose a partially written snapshot.
	pending := filepath.Join(snapshotDir, ".pending-"+rand.Text())
	out, err := task.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer task.Remove(pending)
	_, writeErr := io.Copy(out, bytes.NewReader(data))
	err = errors.Join(writeErr, out.Sync(), out.Close())
	if err != nil {
		return "", err
	}
	if err := task.Chtimes(pending, info.ModTime(), info.ModTime()); err != nil {
		return "", err
	}
	if err := task.Link(pending, snapshot); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	// A preexisting digest path is never trusted just because its name matches.
	existing, err := openRegularScreenshot(task, snapshot)
	if err != nil {
		return "", err
	}
	verifyErr := verifyReportSnapshot(existing, snapshot)
	err = errors.Join(verifyErr, existing.Close())
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(snapshot), nil
}

// archiveFindingScreenshots is the legacy materialization helper. Production
// report writes use prepareFindingReportScreenshots's immutable rewritten URLs.
// into its task archive. Caller validates the conversation/finding association;
// the filesystem capabilities independently enforce both directory boundaries.
func (s *Server) archiveFindingScreenshots(taskID, conversationID int64, markdown string) error {
	if taskID <= 0 || conversationID <= 0 {
		return nil
	}
	for _, match := range screenshotMarkdownLink.FindAllStringSubmatch(markdown, -1) {
		rel, ok := reportScreenshotRelative(match[2])
		if !ok {
			continue
		}
		in, _, err := openReportScreenshot(s.m.dir, 0, conversationID, filepath.ToSlash(rel))
		if err != nil {
			continue
		}
		info, statErr := in.Stat()
		if statErr != nil {
			in.Close()
			return statErr
		}
		task, err := openScreenshotDirectoryRoot(s.m.dir, "tasks", strconv.FormatInt(taskID, 10), true)
		if err != nil {
			in.Close()
			return err
		}
		if err := task.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
			in.Close()
			task.Close()
			return err
		}
		out, err := task.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			// Never overwrite historical evidence or follow a planted symlink.
			existing, openErr := openRegularScreenshot(task, rel)
			if openErr == nil {
				existing.Close()
			}
			in.Close()
			task.Close()
			if openErr != nil {
				return openErr
			}
			continue
		}
		if err != nil {
			in.Close()
			task.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		err = errors.Join(copyErr, out.Close(), in.Close())
		if err == nil {
			err = task.Chtimes(rel, info.ModTime(), info.ModTime())
		}
		if err != nil {
			_ = task.Remove(rel)
		}
		task.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// POST /api/workspace/upload?path=<dir> — multipart form field "file" (one or more).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	dirAbs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	if fi, err := os.Stat(dirAbs); err != nil || !fi.IsDir() {
		writeErr(w, 400, "目标目录不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析上传失败或超出大小限制："+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "缺少上传文件(表单字段 file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." {
			continue
		}
		destAbs, okd := s.wsResolve(filepath.Join(s.wsRel(dirAbs), name))
		if !okd {
			continue
		}
		if err := saveUpload(hdr, destAbs); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

func saveUpload(hdr *multipart.FileHeader, dest string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename token.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}
