package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ReplaySource identifies the configuration that produced signed content.
// Endpoints and credentials are represented by hashes, never secrets.
type ReplaySource struct {
	Format       Format `json:"format"`
	Model        string `json:"model"`
	EndpointHash string `json:"endpoint_hash"`
	AccountHash  string `json:"account_hash"`
	APIVersion   string `json:"api_version,omitempty"`
}

type ReplayMetadata struct {
	Source     *ReplaySource `json:"source,omitempty"`
	Incomplete bool          `json:"incomplete,omitempty"`
}

func (p *anthropicProvider) replaySource() *ReplaySource {
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	return &ReplaySource{Format: FormatAnthropic, Model: p.cfg.Model,
		EndpointHash: hash(strings.TrimRight(p.cfg.BaseURL, "/")),
		AccountHash:  hash(p.cfg.APIKey), APIVersion: p.cfg.APIVersion}
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Opaque) > 0 {
		return json.Marshal(b.Opaque)
	}
	type plain ContentBlock
	// These required fields must survive even when their contents are empty.
	switch b.Type {
	case BlockText:
		return json.Marshal(struct {
			Type BlockType `json:"type"`
			Text string    `json:"text"`
		}{b.Type, b.Text})
	case BlockThinking:
		return json.Marshal(struct {
			Type      BlockType `json:"type"`
			Thinking  string    `json:"thinking"`
			Signature string    `json:"signature,omitempty"`
		}{b.Type, b.Thinking, b.Signature})
	default:
		return json.Marshal(plain(b))
	}
}

func (b *ContentBlock) UnmarshalJSON(raw []byte) error {
	type plain ContentBlock
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*b = ContentBlock(decoded)
	switch b.Type {
	case BlockText, BlockThinking, BlockToolUse, BlockToolResult:
	default:
		b.Opaque = append(json.RawMessage(nil), raw...)
	}
	return nil
}

func IsReasoningBlock(b ContentBlock) bool {
	return b.Type == BlockThinking || b.Type == BlockRedactedThinking
}

func validateReplay(msgs []Message, source *ReplaySource) error {
	for i, m := range msgs {
		if m.Replay != nil && m.Replay.Incomplete {
			return &HistoryError{Reason: "incomplete model response", MessageIndex: i, Recoverable: true}
		}
		for _, b := range m.Content {
			if !IsReasoningBlock(b) {
				switch b.Type {
				case BlockText, BlockToolUse, BlockToolResult:
					continue
				}
				return &HistoryError{Reason: "unsupported history block " + string(b.Type), MessageIndex: i, Recoverable: true}
			}
			if m.Replay != nil && m.Replay.Source != nil && *m.Replay.Source != *source {
				return &HistoryError{Reason: "reasoning source changed", MessageIndex: i, Recoverable: true}
			}
			// Older text thinking has no provenance: preserve it for provider
			// validation, without treating it as verified. Opaque reasoning has
			// no portable interpretation and requires the original configuration.
			if b.Type == BlockRedactedThinking && (m.Replay == nil || m.Replay.Source == nil) {
				return &HistoryError{Reason: "opaque reasoning source unknown", MessageIndex: i, Recoverable: true}
			}
		}
	}
	return nil
}
