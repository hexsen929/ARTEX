package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

func writeReportScreenshotFixture(t *testing.T, root, path string, data []byte) string {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReportScreenshotArchiveUsesOnlyCurrentConversation(t *testing.T) {
	root := t.TempDir()
	rel := "findings/screenshots/finding-7-step-1.png"
	want := []byte("current-conversation screenshot")
	source := writeReportScreenshotFixture(t, root, "sessions/conv-11/"+rel, want)
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	writeReportScreenshotFixture(t, root, "sessions/conv-99/"+rel, []byte("newer unrelated screenshot"))
	s := &Server{m: &Manager{dir: root}}
	if err := s.archiveFindingScreenshots(3, 11, "![截图]("+rel+")"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "tasks", "3", filepath.FromSlash(rel))
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("wrong report screenshot archived: %q %v", got, err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("capture timestamp was replaced: %v %v", info, err)
	}
	// Missing/unknown source identity never falls back to another conversation.
	for _, conversationID := range []int64{0, 12} {
		if err := s.archiveFindingScreenshots(4, conversationID, "![截图]("+rel+")"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "tasks", "4", filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("unknown source copied another session: %v", err)
		}
	}
}

func TestReportScreenshotArchiveRejectsAliasesAndTraversal(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	rel := "findings/screenshots/fixture.png"
	secret := []byte("outside content must stay outside")
	outsideFile := writeReportScreenshotFixture(t, outside, rel, secret)
	s := &Server{m: &Manager{dir: root}}
	if err := os.MkdirAll(filepath.Join(root, "sessions", "conv-10", "findings", "screenshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "sessions", "conv-10", filepath.FromSlash(rel))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sessions", "conv-11")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{10, 11} {
		if err := s.archiveFindingScreenshots(3, id, "![截图]("+rel+")"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "3", filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("source symlink was archived: %v", err)
	}
	// A session alias to another in-root session is still the wrong identity.
	writeReportScreenshotFixture(t, root, "sessions/conv-12/"+rel, secret)
	if err := os.Symlink("conv-12", filepath.Join(root, "sessions", "conv-13")); err != nil {
		t.Fatal(err)
	}
	if err := s.archiveFindingScreenshots(3, 13, "![截图]("+rel+")"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "3", filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("another session alias was archived: %v", err)
	}
	// Neither an outside nor an in-root task alias is an acceptable destination.
	writeReportScreenshotFixture(t, root, "sessions/conv-14/"+rel, []byte("valid source"))
	if err := os.MkdirAll(filepath.Join(root, "tasks", "6"), 0o700); err != nil {
		t.Fatal(err)
	}
	for task, target := range map[string]string{"4": outside, "5": "6"} {
		if err := os.Symlink(target, filepath.Join(root, "tasks", task)); err != nil {
			t.Fatal(err)
		}
		id, _ := strconv.ParseInt(task, 10, 64)
		if err := s.archiveFindingScreenshots(id, 14, "![截图]("+rel+")"); err == nil {
			t.Fatalf("aliased task %s accepted screenshot writes", task)
		}
	}
	for _, invalid := range []string{"findings/screenshots/../fixture.png", "findings/screenshots/../../other.png", "file://" + outsideFile, filepath.Join(outside, rel)} {
		if err := s.archiveFindingScreenshots(7, 14, "![截图]("+invalid+")"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "7")); !os.IsNotExist(err) {
		t.Fatalf("untrusted report path caused writes: %v", err)
	}
	got, err := os.ReadFile(outsideFile)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatal("outside screenshot was modified", err)
	}
}

func TestReportScreenshotExportIsFindingScopedAndRechecksHandles(t *testing.T) {
	root := t.TempDir()
	rel := "findings/screenshots/报告截图.png"
	taskID, conversationID := int64(3), int64(11)
	finding := &db.DBFinding{ID: 7, TaskID: &taskID, ReportConversationID: &conversationID}
	want := []byte("own report screenshot")
	source := writeReportScreenshotFixture(t, root, "tasks/3/"+rel, want)
	writeReportScreenshotFixture(t, root, "tasks/4/"+rel, []byte("other task screenshot"))
	writeReportScreenshotFixture(t, root, "sessions/conv-99/"+rel, []byte("newer unrelated session screenshot"))
	markdown, screenshots, err := prepareFindingExportReport("![步骤]("+rel+")", finding, root)
	if err != nil || len(screenshots) != 1 || !strings.Contains(markdown, "screenshots/7/报告截图.png") {
		t.Fatalf("own report screenshot not prepared: %q %+v %v", markdown, screenshots, err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	item := screenshots[0]
	if err := copyScreenshotZipFile(zw, item.ArchivePath, item.SourcePath, root, item.taskID); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil || len(zr.File) != 1 {
		t.Fatalf("invalid report screenshot archive: %v", err)
	}
	entry, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(entry)
	entry.Close()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("wrong task screenshot exported: %q %v", got, err)
	}
	// Explicit paths cannot name a different task or report conversation.
	for _, path := range []string{"tasks/4/" + rel, "workspace://sessions/conv-99/" + rel, filepath.Join(root, "tasks", "4", filepath.FromSlash(rel))} {
		_, items, err := prepareFindingExportReport("![步骤]("+path+")", finding, root)
		if err != nil || len(items) != 0 {
			t.Fatalf("cross-owner screenshot accepted: %q %+v %v", path, items, err)
		}
	}
	// A source replaced after manifest preparation is checked again when copied.
	outside := writeReportScreenshotFixture(t, t.TempDir(), "secret.png", []byte("secret outside evidence"))
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, source); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var rejected bytes.Buffer
	badZip := zip.NewWriter(&rejected)
	if err := copyScreenshotZipFile(badZip, item.ArchivePath, item.SourcePath, root, item.taskID); err == nil {
		t.Fatal("export followed a source symlink planted after preparation")
	}
	badZip.Close()
	if bytes.Contains(rejected.Bytes(), []byte("secret outside evidence")) {
		t.Fatal("outside content reached archive")
	}
	// Export has no live DB authority: even a matching conversation pointer
	// cannot authorize session fallback. Normal report writes archive it first.
	os.Remove(source)
	writeReportScreenshotFixture(t, root, "sessions/conv-11/"+rel, want)
	_, items, err := prepareFindingExportReport("![步骤]("+rel+")", finding, root)
	if err != nil || len(items) != 0 {
		t.Fatalf("bare conversation pointer authorized export: %+v %v", items, err)
	}
	s := &Server{m: &Manager{dir: root}}
	if err := s.archiveFindingScreenshots(taskID, conversationID, "![步骤]("+rel+")"); err != nil {
		t.Fatal(err)
	}
	_, items, err = prepareFindingExportReport("![步骤]("+rel+")", finding, root)
	if err != nil || len(items) != 1 || !strings.HasPrefix(items[0].SourcePath, "tasks/3/") {
		t.Fatalf("archived reporter screenshot unavailable: %+v %v", items, err)
	}
}

func TestReportScreenshotPreviewUsesFindingTaskAndBoundConversation(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	s := &Server{m: m}
	create := func(name string) (task *db.Task, findingID, nodeID int64) {
		t.Helper()
		task, err = m.pg.CreateTask(name, "screenshot isolation fixture", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			m.pg.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID)
			m.pg.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
			m.pg.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
		})
		nodeID, err = m.pg.Exploration(task.ExplorationID).AddNode("finding", map[string]any{"summary": name}, 0, "confirmed", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		findingID, err = m.pg.AddFinding(task.ID, nodeID, "fixture", name, "low", name, "fixture", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return task, findingID, nodeID
	}
	a, fa, nodeA := create("screenshot preview A")
	b, fb, _ := create("screenshot preview B")
	rel := "findings/screenshots/arbitrary.png"
	own, other := []byte("\x89PNG\r\n\x1a\nowner screenshot"), []byte("\x89PNG\r\n\x1a\nother task screenshot")
	writeReportScreenshotFixture(t, m.dir, fmt.Sprintf("tasks/%d/%s", a.ID, rel), own)
	writeReportScreenshotFixture(t, m.dir, fmt.Sprintf("tasks/%d/%s", b.ID, rel), other)
	writeReportScreenshotFixture(t, m.dir, "sessions/conv-999999/"+rel, []byte("unrelated latest screenshot"))
	preview := func(path string, findingID string) *httptest.ResponseRecorder {
		t.Helper()
		values := url.Values{"path": {path}}
		if findingID != "" {
			values.Set("finding_id", findingID)
		}
		w := httptest.NewRecorder()
		s.wsPreview(w, httptest.NewRequest(http.MethodGet, "/api/workspace/preview?"+values.Encode(), nil))
		return w
	}
	for id, want := range map[int64][]byte{fa: own, fb: other} {
		w := preview(rel, strconv.FormatInt(id, 10))
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatalf("finding %d preview mixed task screenshots: status=%d body=%q", id, w.Code, w.Body.Bytes())
		}
	}
	if w := preview(rel, ""); w.Code != http.StatusNotFound {
		t.Fatalf("contextless arbitrary screenshot guessed a session: %d", w.Code)
	}
	if w := preview(rel, "-1"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid finding ID was accepted: %d", w.Code)
	}
	canonical := fmt.Sprintf("findings/screenshots/finding-%d-step-1.png", nodeA)
	writeReportScreenshotFixture(t, m.dir, fmt.Sprintf("tasks/%d/%s", a.ID, canonical), own)
	writeReportScreenshotFixture(t, m.dir, fmt.Sprintf("tasks/%d/%s", b.ID, canonical), other)
	if w := preview(canonical, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), own) {
		t.Fatalf("native-node compatibility selected wrong screenshot: %d %q", w.Code, w.Body.Bytes())
	}
	conversation, err := m.pg.CreateConversation("reporter", "screenshot preview fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conversation.ID) })
	if err := m.pg.SetFindingReportConversationID(fa, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.dir, "tasks", strconv.FormatInt(a.ID, 10), filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	explicit := fmt.Sprintf("sessions/conv-%d/%s", conversation.ID, rel)
	writeReportScreenshotFixture(t, m.dir, explicit, own)
	if w := preview(rel, strconv.FormatInt(fa, 10)); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), own) {
		t.Fatalf("bound reporter screenshot not resolved: %d %q", w.Code, w.Body.Bytes())
	}
	if w := preview(explicit, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), own) {
		t.Fatalf("explicit legacy session path no longer previews: %d %q", w.Code, w.Body.Bytes())
	}
	foreign, err := m.pg.CreateConversation("reporter", "foreign screenshot fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, foreign.ID) })
	writeReportScreenshotFixture(t, m.dir, fmt.Sprintf("sessions/conv-%d/%s", foreign.ID, rel), other)
	if _, err := m.pg.Exec(`UPDATE findings SET report_conversation_id=$2 WHERE id=$1`, fa, foreign.ID); err != nil {
		t.Fatal(err)
	}
	if w := preview(rel, strconv.FormatInt(fa, 10)); w.Code != http.StatusNotFound {
		t.Fatalf("unbound old pointer authorized screenshot fallback: %d %q", w.Code, w.Body.Bytes())
	}
	if err := m.pg.SetFindingReportConversationID(fb, foreign.ID); err != nil {
		t.Fatal(err)
	}
	if w := preview(rel, strconv.FormatInt(fa, 10)); w.Code != http.StatusNotFound {
		t.Fatalf("another finding's reporter authorized screenshot fallback: %d %q", w.Code, w.Body.Bytes())
	}
}

func TestReportScreenshotSnapshotsAreImmutableAndKeepDistinctZipEntries(t *testing.T) {
	root := t.TempDir()
	s := &Server{m: &Manager{dir: root}}
	rel := "findings/screenshots/finding-7-step-1.png"
	first := []byte("first capture at same basename")
	second := []byte("second capture at same basename")
	writeReportScreenshotFixture(t, root, "sessions/conv-11/"+rel, first)
	body := "![步骤](" + rel + ")"
	one, err := s.prepareFindingReportScreenshots(3, 11, body)
	if err != nil {
		t.Fatal(err)
	}
	writeReportScreenshotFixture(t, root, "sessions/conv-11/"+rel, second)
	two, err := s.prepareFindingReportScreenshots(3, 11, body)
	if err != nil || one == two || !strings.Contains(two, "/.snapshots/") {
		t.Fatalf("new capture reused mutable image URL: one=%q two=%q err=%v", one, two, err)
	}
	// Keeping an already committed snapshot URL does not require a recapture.
	if retained, err := s.prepareFindingReportScreenshots(3, 0, two); err != nil || retained != two {
		t.Fatalf("immutable snapshot URL was not preserved: %q %v", retained, err)
	}
	taskID := int64(3)
	finding := &db.DBFinding{ID: 7, TaskID: &taskID}
	_, screenshots, err := prepareFindingExportReport(one+"\n"+two, finding, root)
	if err != nil || len(screenshots) != 2 || screenshots[0].ArchivePath == screenshots[1].ArchivePath {
		t.Fatalf("different captures collapsed to one ZIP basename: %+v %v", screenshots, err)
	}
	for i, want := range [][]byte{first, second} {
		file, _, err := openReportScreenshot(root, taskID, 0, screenshots[i].SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("snapshot %d changed contents: %q %v", i, got, err)
		}
	}
	// A matching hash-shaped filename is insufficient if its bytes were altered.
	corrupt := filepath.Join(root, filepath.FromSlash(screenshots[1].SourcePath))
	if err := os.WriteFile(corrupt, []byte("corrupt existing snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareFindingReportScreenshots(3, 11, body); err == nil {
		t.Fatal("reused a corrupted snapshot just because its digest path existed")
	}
	if file, _, err := openReportScreenshot(root, taskID, 0, screenshots[1].SourcePath); err == nil {
		file.Close()
		t.Fatal("preview/export opened a snapshot whose contents no longer match its digest")
	}
}

func TestReportScreenshotStaleWriteCannotPoisonLaterAcceptedCapture(t *testing.T) {
	s, recorded, _ := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(recorded.FindingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal("missing screenshot report fixture", finding, err)
	}
	conversation, _, err := s.m.pg.EnsureFindingReportConversation(t.Context(), finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteConversation(conversation.ID) })
	ctx := intercept.WithConvID(t.Context(), conversation.ID)
	rel := fmt.Sprintf("findings/screenshots/finding-%d-step-1.png", recorded.NodeID)
	capturePath := fmt.Sprintf("sessions/conv-%d/%s", conversation.ID, rel)
	body := "## 报告\n\n![步骤](" + rel + ")"
	stale := []byte("\x89PNG\r\n\x1a\nstale unaccepted capture")
	writeReportScreenshotFixture(t, s.m.dir, capturePath, stale)
	workflowCall(t, ctx, s.toolUpdateFindingReport(), map[string]any{
		"finding_id": recorded.NodeID, "report": body, "evidence_version": finding.EvidenceVersion + 1,
	}, true)
	after, err := s.m.pg.GetFinding(finding.ID)
	if err != nil || after.Report != finding.Report {
		t.Fatal("stale report modified persisted body", after, err)
	}
	// A failed preparation must not occupy the old mutable task basename.
	legacy := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(*finding.TaskID, 10), filepath.FromSlash(rel))
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("stale report polluted the reusable task basename: %v", err)
	}
	var previous string
	for _, want := range [][]byte{
		[]byte("\x89PNG\r\n\x1a\naccepted second capture"),
		[]byte("\x89PNG\r\n\x1a\naccepted third capture"),
	} {
		writeReportScreenshotFixture(t, s.m.dir, capturePath, want)
		workflowCall(t, ctx, s.toolUpdateFindingReport(), map[string]any{
			"finding_id": recorded.NodeID, "report": body, "evidence_version": finding.EvidenceVersion,
		}, false)
		after, err = s.m.pg.GetFinding(finding.ID)
		if err != nil {
			t.Fatal(err)
		}
		match := screenshotMarkdownLink.FindStringSubmatch(after.Report)
		if len(match) != 3 || match[2] == rel || match[2] == previous || !strings.Contains(match[2], "/.snapshots/") {
			t.Fatalf("accepted report did not name its exact new capture: %q", after.Report)
		}
		previous = match[2]
		request := "/api/workspace/preview?path=" + url.QueryEscape(match[2]) + "&finding_id=" + strconv.FormatInt(finding.ID, 10)
		preview := httptest.NewRecorder()
		s.wsPreview(preview, httptest.NewRequest(http.MethodGet, request, nil))
		if preview.Code != http.StatusOK || !bytes.Equal(preview.Body.Bytes(), want) {
			t.Fatalf("accepted report displayed rejected/earlier capture: %d %q", preview.Code, preview.Body.Bytes())
		}
		_, images, err := prepareFindingExportReport(after.Report, after, s.m.dir)
		if err != nil || len(images) != 1 {
			t.Fatalf("accepted snapshot not exportable: %+v %v", images, err)
		}
		var archive bytes.Buffer
		zw := zip.NewWriter(&archive)
		if err := copyScreenshotZipFile(zw, images[0].ArchivePath, images[0].SourcePath, s.m.dir, images[0].taskID); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
		if err != nil || len(zr.File) != 1 {
			t.Fatalf("invalid accepted screenshot ZIP: %v", err)
		}
		reader, err := zr.File[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(reader)
		reader.Close()
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("accepted report ZIP reused stale capture: %q %v", got, readErr)
		}
	}
}
