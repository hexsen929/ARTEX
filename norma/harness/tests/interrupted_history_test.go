//go:build ignore

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"io"
	"iter"
	"strings"
	"testing"
)

func TestInterruptedStreamRecordsCompletedToolsAndSkipsPartial(t *testing.T) {
	runs := 0
	probe := tool.Build(tool.Spec{Name: "write_probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		runs++
		return tool.Text("written once"), nil
	}})
	deps := QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEToolUseStart, Indexed: true, BlockIndex: 0, ToolID: "complete", ToolName: "write_probe"},
				{Type: llm.SEToolInputJSON, Indexed: true, BlockIndex: 0, Text: `{}`},
				{Type: llm.SEContentBlockStop, Indexed: true, BlockIndex: 0},
				{Type: llm.SEToolUseStart, Indexed: true, BlockIndex: 1, ToolID: "partial", ToolName: "write_probe"},
				{Type: llm.SEToolInputJSON, Indexed: true, BlockIndex: 1, Text: `{"path":`},
			} {
				if !yield(ev, nil) {
					return
				}
			}
			yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
		}
	}}
	rec := &capRecorder{}
	term := drain(t, QueryInput{Messages: []llm.Message{llm.UserText("write")}, Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, Recorder: rec}, deps)
	if !errors.Is(term.Err, io.ErrUnexpectedEOF) || runs != 1 {
		t.Fatalf("runs=%d terminal=%+v", runs, term)
	}
	if len(rec.msgs) != 2 || rec.msgs[0].Replay == nil || !rec.msgs[0].Replay.Incomplete || rec.msgs[1].Content[0].ToolUseID != "complete" {
		t.Fatalf("lost partial turn/results: %+v", rec.msgs)
	}
	if !strings.Contains(rec.msgs[1].Content[0].Content[0].Text, "written once") {
		t.Fatal("lost completed side effect")
	}
	if rec.msgs[0].Content[1].IncompleteInput != `{"path":` {
		t.Fatalf("partial input lost: %+v", rec.msgs[0])
	}
}
