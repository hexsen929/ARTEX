package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type sideAgentProvider struct {
	calls int
	path  string
	infos []RunInfo
}

func (p *sideAgentProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				if !y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			} else if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}
func (p *sideAgentProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	p.infos = append(p.infos, RunInfoFrom(ctx))
	if p.calls == 1 {
		input, _ := json.Marshal(map[string]string{"file_path": p.path})
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "fixture-read", Name: "Read", Input: input}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("main finished")}}, "end_turn", llm.Usage{}, nil
}

func TestSideCaptureParentChoosesConversationWithoutChangingTaskContext(t *testing.T) {
	info := RunInfo{TaskID: 11, ExplorationID: 13, IntentID: 17, SessionID: "conv-19"}
	parent, ok := sideCaptureParent(info)
	if !ok || parent != (sidequestion.Parent{ConversationID: 19}) || info.TaskID != 11 || info.ExplorationID != 13 || info.IntentID != 17 {
		t.Fatalf("mixed conversation identity: %+v %+v %v", info, parent, ok)
	}
	for _, sessionID := range []string{"conv-", "conv-invalid", "conv-0", "conv--1", "conv-+1", "conv-01", "conv-1-tail", "conv-9223372036854775808"} {
		info.SessionID = sessionID
		if got, ok := sideCaptureParent(info); ok {
			t.Fatalf("malformed conversation fell back to task: %q %+v", sessionID, got)
		}
	}
	info.SessionID = "task-run"
	if got, ok := sideCaptureParent(info); !ok || got != (sidequestion.Parent{TaskID: 11, ExplorationID: 13, IntentID: 17}) {
		t.Fatalf("task identity changed: %+v %v", got, ok)
	}
	if got, ok := sideCaptureParent(RunInfo{}); ok {
		t.Fatalf("empty identity accepted: %+v", got)
	}
}

func TestTaskAssociatedVerifierSideCapturePersistsConversationOnly(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "atomic"}[streaming], func(t *testing.T) {
			dsn, _, err := db.DSN()
			if err != nil {
				t.Fatal(err)
			}
			pg, err := db.Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pg.Close() })
			task, err := pg.CreateTask("side capture verifier fixture", "fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
			intentID, err := pg.Exploration(task.ExplorationID).AddNode("intent", map[string]any{"summary": "fixture"}, 0, "running", "planner", nil)
			if err != nil {
				t.Fatal(err)
			}
			conversation, err := pg.CreateConversation("verifier", "side capture verifier fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pg.DeleteConversation(conversation.ID) })
			dir := t.TempDir()
			file := filepath.Join(dir, "asset.txt")
			if err := os.WriteFile(file, []byte("verifier-local-fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			provider := &sideAgentProvider{path: file}
			bound := sidequestion.Bind(provider, sidequestion.Model{Model: "fixture", Streaming: streaming})
			chat := NewChatAgent(bound, "fixture", dir, nil, 100000)
			chat.SetNonStreaming(func() bool { return !streaming })
			original := RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intentID, SessionID: "parent-run"}
			parentCtx := WithRunInfo(t.Context(), original)
			var snapshots []sidequestion.Snapshot
			ctx := sidequestion.WithPublisher(parentCtx, func(snapshot sidequestion.Snapshot) {
				if snapshot.Parent != (sidequestion.Parent{ConversationID: conversation.ID}) {
					t.Errorf("checkpoint inherited mixed task identity: %+v", snapshot.Parent)
				}
				if err := pg.SaveSideSnapshot(t.Context(), snapshot); err != nil {
					t.Errorf("persist captured checkpoint: %v", err)
				}
				snapshots = append(snapshots, snapshot)
			})
			sessionID := fmt.Sprintf("conv-%d", conversation.ID)
			if _, err := chat.Chat(ctx, "verifier", sessionID, "Read the local fixture", 5, time.Minute, false, nil); err != nil {
				t.Fatal(err)
			}
			if len(snapshots) < 3 || RunInfoFrom(parentCtx) != original {
				t.Fatalf("checkpoint capture/parent context changed: snapshots=%d info=%+v", len(snapshots), RunInfoFrom(parentCtx))
			}
			if len(provider.infos) == 0 {
				t.Fatal("fixture provider was not called")
			}
			for _, info := range provider.infos {
				if info.TaskID != original.TaskID || info.ExplorationID != original.ExplorationID || info.IntentID != original.IntentID || info.SessionID != sessionID {
					t.Fatalf("audit/tool model context lost task attribution: %+v", info)
				}
			}
			stored, err := pg.SideSnapshot(t.Context(), sessionID)
			if err != nil || stored == nil || stored.Parent != (sidequestion.Parent{ConversationID: conversation.ID}) {
				t.Fatalf("stored conversation checkpoint: %+v %v", stored, err)
			}
			var exclusive bool
			if err := pg.QueryRow(`SELECT conversation_id=$2 AND task_id IS NULL AND exploration_id IS NULL AND intent_id IS NULL
FROM side_question_sessions WHERE session_key=$1`, sessionID, conversation.ID).Scan(&exclusive); err != nil || !exclusive {
				t.Fatalf("stored parent columns violate exclusive identity: %v %v", exclusive, err)
			}
		})
	}
}

func TestSideActualChatCheckpointToolResultAndTranscriptIsolation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "atomic"}[streaming], func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "asset.txt")
			if err := os.WriteFile(file, []byte("controlled-homepage-result"), 0600); err != nil {
				t.Fatal(err)
			}
			p := &sideAgentProvider{path: file}
			bound := sidequestion.Bind(p, sidequestion.Model{Model: "fixture", Streaming: streaming})
			store := transcript.NewStore(filepath.Join(dir, "transcripts"))
			chat := NewChatAgent(bound, "fixture", dir, store, 100000)
			chat.SetNonStreaming(func() bool { return !streaming })
			var snapshots []sidequestion.Snapshot
			var activities []db.Activity
			ctx := sidequestion.WithPublisher(t.Context(), func(s sidequestion.Snapshot) { snapshots = append(snapshots, s) })
			if _, err := chat.Chat(ctx, "mainagent", "conv-987654", "Read the local fixture", 5, time.Minute, false, func(a db.Activity) { activities = append(activities, a) }); err != nil {
				t.Fatal(err)
			}
			if len(snapshots) < 3 {
				t.Fatalf("actual Chat missed capture hooks: %d", len(snapshots))
			}
			last := snapshots[len(snapshots)-1]
			raw, _ := json.Marshal(last)
			if last.Parent.ConversationID != 987654 || !strings.Contains(string(raw), "controlled-homepage-result") || !strings.Contains(string(raw), "main finished") {
				t.Fatalf("missing real tool result/final reply: %s", raw)
			}
			before, err := os.ReadFile(store.MainPath("conv-987654"))
			if err != nil {
				t.Fatal(err)
			}
			count := len(activities)
			tools := 0
			for _, a := range activities {
				if a.Kind == "tool_use" {
					tools++
				}
			}
			if tools != 1 {
				t.Fatalf("main fixture tool executions: %d", tools)
			}
			req, err := sidequestion.BuildRequest(last, nil, "side-only-question")
			if err != nil {
				t.Fatal(err)
			}
			// Reset the fake to request another Read; the side executor cannot run it.
			p.calls = 0
			p.path = filepath.Join(dir, "nonexistent")
			answer, err := (sidequestion.SideQuestionService{Provider: bound}).Answer(t.Context(), req, streaming, nil)
			if err != nil || !answer.ToolUse || p.calls != 1 || !strings.Contains(answer.Text, "不能执行工具") {
				t.Fatalf("tool denial %+v %v", answer, err)
			}
			after, err := os.ReadFile(store.MainPath("conv-987654"))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || len(activities) != count {
				t.Fatal("side question modified main transcript/activity")
			}
			if len(snapshots) == 0 || snapshots[len(snapshots)-1].Version != last.Version {
				t.Fatal("side request replaced main checkpoint")
			}
		})
	}
}
