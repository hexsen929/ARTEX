//go:build ignore

package noaadapter

import (
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"reflect"
	"testing"
)

func TestSignedThinkingCannotBeRewritten(t *testing.T) {
	original := []llm.Message{assistantWith(llm.ContentBlock{Type: llm.BlockThinking, Thinking: "signed original", Signature: "signature"}, llm.TextBlock("answer"))}
	cores, sc := Project(original)
	cores[0].Text = "compressed reasoning"
	view := Reassemble(cores, original, sc, ReassembleOptions{})
	if !reflect.DeepEqual(view, original) {
		t.Fatalf("signed payload changed: %+v", view)
	}
}

func TestOpaqueAndReplayMetadataSurviveProjection(t *testing.T) {
	var b llm.ContentBlock
	json.Unmarshal([]byte(`{"type":"redacted_thinking","data":"secret opaque bytes"}`), &b)
	original := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{b}, Replay: &llm.ReplayMetadata{Source: &llm.ReplaySource{Format: llm.FormatAnthropic, Model: "m"}}}}
	cores, sc := Project(original)
	if len(cores) != 1 {
		t.Fatalf("opaque silently dropped: %+v", cores)
	}
	cores[0].Text = "rewritten opaque"
	view := Reassemble(cores, original, sc, ReassembleOptions{})
	if !reflect.DeepEqual(view, original) {
		t.Fatalf("opaque/source changed: %+v", view)
	}
}
