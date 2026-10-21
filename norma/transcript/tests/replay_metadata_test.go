//go:build ignore

package transcript

import (
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"reflect"
	"strings"
	"testing"
)

func TestReplayMetadataSurvivesColdLoad(t *testing.T) {
	store := NewStore(t.TempDir())
	writer := store.NewWriter("replay", "")
	var opaque llm.ContentBlock
	json.Unmarshal([]byte(`{"type":"redacted_thinking","data":"unmodified"}`), &opaque)
	want := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{opaque, llm.TextBlock("partial")}, Replay: &llm.ReplayMetadata{Source: &llm.ReplaySource{Format: llm.FormatAnthropic, Model: "m", EndpointHash: "hash", AccountHash: "account"}, Incomplete: true}}
	writer.RecordMessage(want, llm.Usage{OutputTokens: 4})
	if err := writer.Err(); err != nil {
		t.Fatal(err)
	}
	records, err := store.Load(store.MainPath("replay"))
	if err != nil {
		t.Fatal(err)
	}
	msgs, usage := Messages(records)
	if len(msgs) != 1 || !reflect.DeepEqual(msgs[0], want) || usage.OutputTokens != 4 {
		t.Fatalf("lost metadata: %+v %+v", msgs, usage)
	}
	wire, _ := json.Marshal(msgs[0])
	if strings.Contains(string(wire), "endpoint_hash") || strings.Contains(string(wire), "incomplete") {
		t.Fatalf("metadata in wire: %s", wire)
	}
}
