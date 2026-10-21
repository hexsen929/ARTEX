package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RequestError identifies deterministic local request/configuration errors.
// Retrying them against another profile does not repair the request.
type RequestError struct{ Field, Value string }

func (e *RequestError) Error() string { return fmt.Sprintf("invalid %s %q", e.Field, e.Value) }

// HistoryError identifies a rejected replay, before any response is delivered.
// Recoverable errors may start a new context using recorded facts; malformed
// tool associations require explicit repair and must not imply tool completion.
type HistoryError struct {
	Reason       string
	MessageIndex int
	ToolID       string
	Recoverable  bool
	Cause        error
}

func (e *HistoryError) Error() string {
	return fmt.Sprintf(
		"anthropic history: %s at message %d (tool %q); restore original history or start a context from recorded results",
		e.Reason, e.MessageIndex, e.ToolID,
	)
}
func (e *HistoryError) Unwrap() error { return e.Cause }

func resolveThinkingMode(config, override string) (string, error) {
	mode := config
	if override != "" {
		mode = override
	}
	switch mode {
	case "", "enabled", "disabled":
		return mode, nil
	}
	return "", &RequestError{Field: "thinking mode", Value: mode}
}

// RestoreThinking copies complete, original blocks only when the non-thinking
// assistant payload matches exactly. Tool IDs alone cannot authenticate a turn.
func RestoreThinking(view, original []Message) []Message {
	out := append([]Message(nil), view...)
	for i, m := range view {
		if m.Role != RoleAssistant || hasThinking(m) {
			continue
		}
		payload, err := json.Marshal(withoutThinking(m.Content))
		if err != nil {
			continue
		}
		var match *Message
		ambiguous := false
		for j := range original {
			candidate := original[j]
			if candidate.Role != RoleAssistant || !hasThinking(candidate) {
				continue
			}
			other, err := json.Marshal(withoutThinking(candidate.Content))
			if err != nil || !bytes.Equal(payload, other) {
				continue
			}
			if match != nil {
				left, leftErr := json.Marshal(struct {
					Content []ContentBlock
					Replay  *ReplayMetadata
				}{match.Content, match.Replay})
				right, rightErr := json.Marshal(struct {
					Content []ContentBlock
					Replay  *ReplayMetadata
				}{candidate.Content, candidate.Replay})
				if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
					ambiguous = true
					break
				}
			}
			match = &original[j]
		}
		if match != nil && !ambiguous {
			out[i].Content = append([]ContentBlock(nil), match.Content...)
			out[i].Replay = match.Replay
		}
	}
	return out
}

func withoutThinking(blocks []ContentBlock) []ContentBlock {
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if !IsReasoningBlock(b) {
			out = append(out, b)
		}
	}
	return out
}

func hasThinking(m Message) bool {
	for _, b := range m.Content {
		if IsReasoningBlock(b) {
			return true
		}
	}
	return false
}

func validateAnthropicHistory(msgs []Message, mode string) error {
	pending := make(map[string]int)
	seen := make(map[string]bool)
	for i, m := range msgs {
		if len(pending) > 0 && m.Role != RoleUser {
			return &HistoryError{Reason: "missing tool result", MessageIndex: i}
		}
		for _, b := range m.Content {
			if b.IncompleteInput != "" {
				return &HistoryError{Reason: "incomplete model response", MessageIndex: i, ToolID: b.ID, Recoverable: true}
			}
			switch b.Type {
			case BlockToolUse:
				if m.Role != RoleAssistant || b.ID == "" || seen[b.ID] {
					return &HistoryError{Reason: "invalid or duplicate tool call", MessageIndex: i, ToolID: b.ID}
				}
				pending[b.ID], seen[b.ID] = i, true
			case BlockToolResult:
				if _, ok := pending[b.ToolUseID]; !ok || m.Role != RoleUser {
					return &HistoryError{Reason: "orphan or duplicate tool result", MessageIndex: i, ToolID: b.ToolUseID}
				}
				delete(pending, b.ToolUseID)
			}
		}
		if m.Role == RoleUser && len(pending) > 0 {
			return &HistoryError{Reason: "incomplete tool results", MessageIndex: i}
		}
	}
	if len(pending) > 0 {
		return &HistoryError{Reason: "missing tool result", MessageIndex: len(msgs) - 1}
	}
	// Native Messages requires thinking at the start of assistant tool turns in
	// the current tool round. Default-mode gateways can impose different rules;
	// their explicit rejection is classified below instead of guessing from URLs.
	if mode != "enabled" {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == RoleUser {
			results := false
			for _, b := range m.Content {
				if b.Type == BlockToolResult {
					results = true
				}
			}
			if !results {
				break
			}
		}
		if uses := m.ToolUses(); len(uses) > 0 && m.Content[0].Type != BlockThinking {
			return &HistoryError{Reason: "missing thinking in tool round", MessageIndex: i, ToolID: uses[0].ID, Recoverable: true}
		}
	}
	return nil
}

// classifyAnthropicHistory only recognizes explicit history complaints. Token
// budgets, model capabilities and unrelated 400s retain their original errors.
func classifyAnthropicHistory(err error) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 400 {
		return err
	}
	text := strings.ToLower(httpErr.Body)
	missing := strings.Contains(text, "expected") || strings.Contains(text, "missing") ||
		strings.Contains(text, "must start with")
	history := strings.Contains(text, "thinking") && (strings.Contains(text, "messages") || strings.Contains(text, "tool"))
	badSignature := strings.Contains(text, "signature") && (strings.Contains(text, "invalid") || strings.Contains(text, "required"))
	// Some Messages gateways name the replay field without mentioning messages
	// or tools. Keep this exact history complaint distinct from mode settings.
	requiredReplay := strings.Contains(text, "content[].thinking") && strings.Contains(text, "must be passed back to the api")
	if (missing && history) || badSignature || requiredReplay {
		return &HistoryError{Reason: "provider rejected thinking history", MessageIndex: -1, Recoverable: true, Cause: err}
	}
	return err
}
