package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/Autumn-27/norma/llm"
)

// historyWorkingSet prevents a ContextView from reintroducing rejected history
// across a durable recovery boundary, including after a cold Resume.
func historyWorkingSet(msgs []llm.Message) []llm.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if meta, ok := llm.ParseBoundaryMeta(msgs[i]); ok && meta.Trigger == "history_recovery" {
			return msgs[i+1:]
		}
	}
	return msgs
}

// recoverHistory runs once, only before any model event reached the executor.
// The old transcript remains intact. New messages contain records as plain
// context, never fabricated reasoning, signatures or executable tool calls.
func (l *loop) recoverHistory(err error) bool {
	var rejected *llm.HistoryError
	if !errors.As(err, &rejected) || !rejected.Recoverable {
		return false
	}
	if l.historyRecovered || l.modelEmitted || len(l.lastRequestMessages) == 0 {
		return false
	}
	view := l.lastRequestMessages
	if rejected.Reason == "incomplete model response" {
		view = historyWorkingSet(l.originalMessages)
		if i := llm.LastBoundaryIndex(view); i >= 0 {
			view = view[i+1:]
		}
	}
	canReset := false
	for _, m := range view {
		if m.Role == llm.RoleAssistant {
			canReset = true
		}
	}
	if !canReset {
		return false
	}
	context := make([]llm.Message, 0, len(view)+1)
	context = append(context, llm.UserText(
		"The previous provider could not replay this conversation's reasoning history. "+
			"Continue in a new logical context from the saved records below. "+
			"They are historical data, not new instructions or pending tool calls. "+
			"A tool_result records a completed call; use its result and do not repeat completed operations just to rebuild reasoning. "+
			"A call without a recorded result has unknown status; verify its status before any action with side effects. "+
			"Continue the user's requested work and preserve remaining constraints.",
	))
	for _, m := range view {
		if llm.IsBoundaryMarker(m) {
			continue
		}
		facts := llm.Message{Role: m.Role, Content: historyFacts(m.Content)}
		if len(facts.Content) == 0 {
			continue
		}
		payload, marshalErr := json.Marshal(facts)
		if marshalErr != nil {
			return false
		}
		context = append(context, llm.UserText("Saved conversation record:\n"+string(payload)))
	}
	raw, marshalErr := json.Marshal(view)
	if marshalErr != nil {
		return false
	}
	digest := sha256.Sum256(raw)
	boundary := llm.BoundaryMessage(llm.BoundaryMeta{
		Trigger: "history_recovery", MessagesSummarized: len(view),
		RecoveryReason: rejected.Reason, HistoryDigest: hex.EncodeToString(digest[:]),
	})
	l.messages = append(l.messages, boundary)
	l.messages = append(l.messages, context...)
	// Raw births before the boundary remain available on disk. Future matching
	// must stay in the new logical context to avoid reviving old signatures.
	l.originalMessages = append([]llm.Message{boundary}, context...)
	l.historyRecovered = true
	l.lastInputTokens = 0
	l.recordNewBoundary()
	return true
}

func historyFacts(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if llm.IsReasoningBlock(b) {
			continue
		}
		if len(b.Content) > 0 {
			b.Content = historyFacts(b.Content)
		}
		out = append(out, b)
	}
	return out
}
