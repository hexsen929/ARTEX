package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
)

func TestRuntimeMergeLegacyAndPinnedWorkersKeepCheckedTaskProxy(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer m.Close()
	task, err := m.CreateTask("runtime merge fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	profile := int64(713)
	s := &Server{
		ctx: context.Background(), m: m, engine: NewEngine(m),
		taskAgents: map[string]*taskAgentBundle{},
		provByProfile: map[int64]*provEntry{profile: {
			prov: agent.ErrorProvider(errors.New("fixture model must not be called")),
			cfg:  agent.Config{Model: "fixture"},
		}},
	}
	legacy := s.agentsForTask(task).wk
	pinned, err := s.workerForTaskSlot(task, WorkerSlot{Name: "work#1", ProfileID: profile})
	if err != nil {
		t.Fatal(err)
	}
	proxyFailure := errors.New("fixture task route unavailable")
	m.taskProxyInitErr = proxyFailure
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	for name, worker := range map[string]*agent.Worker{"legacy": legacy, "pinned": pinned} {
		t.Run(name, func(t *testing.T) {
			reason, _, err := worker.Execute(t.Context(), "work#1", id, nil, task.Store, nil, task.Guard, nil, nil, nil)
			if reason != harness.ReasonHookStopped || !errors.Is(err, proxyFailure) || !strings.Contains(err.Error(), "任务代理未就绪") {
				t.Fatalf("worker did not stop before model/tool execution on task route failure: %s %v", reason, err)
			}
		})
	}
}

func TestRuntimeMergeLifecycleKeepsRuntimeAndScopeContext(t *testing.T) {
	started := time.Unix(100, 0)
	task := taskFromPG(&db.Task{
		ID: 7, ExplorationID: 9, WorkerCount: 2, ActiveRunSeconds: 33,
		RunSegmentStartedAt: &started, SourceTaskIDs: []int64{11}, CompanyIDs: []int64{13},
	}, nil, nil, nil, nil)
	if task.Guard == nil || task.WorkerCount != 2 {
		t.Fatal("runtime hydration lost the task guard or worker override")
	}
	before := task.lifecycleSnapshot()
	if before.ActiveRunSeconds != 33 || before.RunSegmentStartedAt != 100 || before.SourceTaskIDs[0] != 11 || before.CompanyIDs[0] != 13 {
		t.Fatalf("hydration lost runtime or inherited scope context: %+v", before)
	}
	task.updateLifecycle(func(state *taskLifecycleState) {
		state.Paused = true
		state.ActiveRunSeconds = 44
		state.RunSegmentStartedAt = 0
	})
	after := task.lifecycleSnapshot()
	if !after.Paused || after.ActiveRunSeconds != 44 || after.RunSegmentStartedAt != 0 || after.SourceTaskIDs[0] != 11 || after.CompanyIDs[0] != 13 {
		t.Fatalf("runtime mirror lost lifecycle or scope context: %+v", after)
	}
}

func TestRuntimeMergeReporterCompatibilityWarningIsReadOnly(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer m.Close()
	reporter, err := m.pg.GetAgentByKey("reporter")
	if err != nil {
		t.Fatal(err)
	}
	created := reporter == nil
	if created {
		reporter, err = m.pg.CreateAgent("reporter", "fixture reporter", "fixture")
		if err != nil {
			t.Fatal(err)
		}
	}
	var originalPrompt sql.NullInt64
	var originalEnabled bool
	if err := m.pg.QueryRow(`SELECT current_prompt_id,enabled FROM agents WHERE id=$1`, reporter.ID).Scan(&originalPrompt, &originalEnabled); err != nil {
		t.Fatal(err)
	}
	version, err := m.pg.SavePrompt(reporter.ID, "Fixture custom instructions: update_finding_reproduction stays a historical reference.", "runtime-merge-fixture", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if created {
			_ = m.pg.DeleteAgent("reporter")
			return
		}
		_, _ = m.pg.Exec(`UPDATE agents SET current_prompt_id=$2,enabled=$3 WHERE id=$1`, reporter.ID, originalPrompt, originalEnabled)
		_, _ = m.pg.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND version=$2`, reporter.ID, version)
	}()
	if _, err := m.pg.Exec(`UPDATE agents SET enabled=false WHERE id=$1`, reporter.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		err := m.pg.QueryRow(`SELECT md5(
		 COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)::text FROM agents a),'[]') ||
		 COALESCE((SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id)::text FROM agent_prompts p),'[]') ||
		 COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY t.key)::text FROM tools t),'[]') ||
		 COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY t.id)::text FROM agent_triggers t),'[]') ||
		 COALESCE((SELECT jsonb_agg(to_jsonb(v) ORDER BY to_jsonb(v)::text)::text FROM agent_visibility v),'[]') ||
		 COALESCE((SELECT jsonb_agg(to_jsonb(s) ORDER BY s.key)::text FROM settings s),'[]'))`).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	(&Server{m: m}).warnDeprecatedReporterWorkflow()
	if after := snapshot(); before != after {
		t.Fatal("compatibility warning rewrote custom prompts, bindings, triggers, visibility or settings")
	}
}

func TestRuntimeMergeMissingPinnedProfileNeverReplaysWholeIntent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer m.Close()
	id := int64(9223372036854775807)
	router := &taskLLMRuntime{s: &Server{m: m}, taskID: "fixture", agentKey: "worker", pinProfileID: &id}
	_, err = router.current()
	if err == nil || !isTaskLLMRuntimeError(err) || retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatalf("missing pinned profile escaped router-owned no-replay boundary: %v", err)
	}
}

func TestRuntimeMergeBrowserSetupPreservesTLSAndCustomProfile(t *testing.T) {
	for _, input := range [][]string{
		{"-y", "@playwright/mcp", "--user-data-dir", "/fixture/profile"},
		{"-y", "@playwright/mcp", "--user-data-dir=/fixture/profile"},
		{"-y", "@playwright/mcp"},
	} {
		before := append([]string(nil), input...)
		got := ensureBrowserMCPArgs(input, true)
		if !reflect.DeepEqual(input, before) {
			t.Fatal("browser setup mutated its input")
		}
		for _, flag := range []string{"--no-sandbox", "--ignore-https-errors"} {
			for _, arg := range got {
				if arg == flag {
					t.Fatalf("browser setup relaxed security with %s", flag)
				}
			}
		}
		if strings.Contains(strings.Join(input, " "), "--user-data-dir") && strings.Contains(strings.Join(got, " "), "--isolated") {
			t.Fatal("browser setup replaced a custom persistent profile")
		}
		if again := ensureBrowserMCPArgs(got, true); !reflect.DeepEqual(got, again) {
			t.Fatal("browser setup is not idempotent")
		}
	}
	explicit := []string{"--headless", "--no-sandbox", "--ignore-https-errors", "--user-data-dir=/fixture/profile"}
	if got := ensureBrowserMCPArgs(explicit, true); !reflect.DeepEqual(got, explicit) {
		t.Fatal("explicit browser flags were discarded")
	}
}

func TestRuntimeMergeGracefulCloseKeepsLastActiveSegment(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer m.Close()
	task, err := m.CreateTask("runtime close fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartTaskRunSegment(task.ID); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	if _, err := m.pg.Exec(`UPDATE tasks SET run_segment_started_at=now()-interval '4 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTaskRunSegment(task.ID); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("shutdown allowed a late segment start: %v", err)
	}
	restarted, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	defer restarted.DeleteTask(task.ID, DeleteTaskOptions{})
	row, err := restarted.pg.GetTask(id)
	if err != nil || row == nil || row.ActiveRunSeconds < 3 || row.RunSegmentStartedAt != nil || row.Paused {
		t.Fatalf("restart lost graceful runtime or changed pause state: task=%+v err=%v", row, err)
	}
	if state := task.lifecycleSnapshot(); state.ActiveRunSeconds != row.ActiveRunSeconds || state.RunSegmentStartedAt != 0 {
		t.Fatalf("shutdown runtime mirror differs from persisted segment: %+v", state)
	}
}

func TestRuntimeMergePreviewUsesConfinedFileHandle(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	image := []byte("\x89PNG\r\n\x1a\nfixture image")
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", "fixture.png"), image, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := []byte("fixture outside-root content")
	outsideFile := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(outsideFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "images", "escape.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s := &Server{m: &Manager{dir: root}}
	for _, path := range []string{"images/fixture.png", filepath.Join(root, "images", "fixture.png")} {
		rec := httptest.NewRecorder()
		s.wsPreview(rec, httptest.NewRequest(http.MethodGet, "/api/workspace/preview?path="+url.QueryEscape(path), nil))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), image) {
			t.Fatalf("normal preview status=%d body=%q", rec.Code, rec.Body.Bytes())
		}
	}
	for _, path := range []string{"images/escape.png", "images/missing.png"} {
		rec := httptest.NewRecorder()
		s.wsPreview(rec, httptest.NewRequest(http.MethodGet, "/api/workspace/preview?path="+url.QueryEscape(path), nil))
		if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), secret) {
			t.Fatalf("preview leaked outside/missing file: status=%d", rec.Code)
		}
	}
	if err := os.MkdirAll(filepath.Join(outside, "findings", "screenshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "findings", "screenshots", "fixture.png"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sessions", "escape")); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.wsPreview(rec, httptest.NewRequest(http.MethodGet, "/api/workspace/preview?path=findings/screenshots/fixture.png", nil))
	if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Fatalf("session fallback escaped root: status=%d", rec.Code)
	}
}
