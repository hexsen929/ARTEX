package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestExecutionPlannerReadsLatestOutputBeyondHistoryPage(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	exp, err := d.CreateExploration("long history fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendActivity(db.Activity{NodeID: &id, Worker: "work#1", Kind: "result", Summary: "OLD_RESULT", Detail: "OLD_RESULT"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO activity(exploration_id,node_id,worker,kind,summary,detail) SELECT $1,$2,'work#1','tool_result','fixture','fixture' FROM generate_series(1,1100)`, exp, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendActivity(db.Activity{NodeID: &id, Worker: "work#1", Kind: "result", Summary: "CURRENT_RESULT", Detail: "CURRENT_RESULT with real current progress"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendActivity(db.Activity{NodeID: &id, Worker: "work#1", Kind: "text", Summary: "EXTRA_TEXT", Detail: "EXTRA_TEXT"}); err != nil {
		t.Fatal(err)
	}
	if got := workerOutput(s, id); !strings.Contains(got, "CURRENT_RESULT") || strings.Contains(got, "OLD_RESULT") {
		t.Fatal("planner read stale summary", got)
	}
	input, _ := json.Marshal(map[string]any{"intent_id": id})
	result, err := NewToolSet(s, "planner").getWorkerOutput().Call(t.Context(), input, nil)
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(result.Flatten()), &out)
	if out["final_text"] != "CURRENT_RESULT with real current progress" || out["output_at"] == nil {
		t.Fatal("tool did not return latest result", out)
	}
}

func TestExecutionCorruptHistoryCannotSilentlyRestartWorker(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	path := store.MainPath("fixture-resume")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{not a valid transcript}\n")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	session := agentcore.NewSession(agentcore.Options{Transcript: store, Tools: []tool.CoreTool{}, SessionID: "fixture-resume"})
	defer session.Close()
	if err := resumeWorkerSession(session, "fixture-resume"); !errors.Is(err, ErrWorkerHistoryUnavailable) {
		t.Fatal("corrupt history was treated as a fresh start", err)
	}
	if len(session.Messages()) != 0 {
		t.Fatal("corrupt history was partially resumed")
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(broken) {
		t.Fatal("corrupt evidence was overwritten")
	}
	if err := resumeWorkerSession(session, "new-intent-with-no-history"); err != nil {
		t.Fatal("new intent could not start", err)
	}
}

func TestExecutionScenarioGuidanceDoesNotUndoEvidenceCalibration(t *testing.T) {
	for _, category := range []string{"meta", "recon", "web"} {
		guidance, ok := chainskel.Skeleton(category)
		if !ok {
			t.Fatal("missing guidance", category)
		}
		for _, unsupported := range []string{"工具阳性可信", "请求没到应用层", "403=存在但禁访", "=WAF 伪造"} {
			if strings.Contains(guidance, unsupported) {
				t.Fatal("scenario guidance overrides evidence limits", category, unsupported)
			}
		}
	}
	if !strings.Contains(workerResumeInput, "上次工具结果缺失或中断") || !strings.Contains(workerResumeInput, "不重做整套准备") {
		t.Fatal("resume lost continuity checks")
	}
}
