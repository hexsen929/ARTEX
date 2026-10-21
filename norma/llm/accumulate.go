package llm

import (
	"encoding/json"
	"strings"
)

// Accumulator reassembles a complete assistant Message from a stream of
// normalized StreamEvents. The harness feeds every event to Add; once the
// stream ends, Message returns the finished assistant turn (text, thinking, and
// tool_use blocks in arrival order).
type Accumulator struct {
	blocks     []ContentBlock
	toolInputs []*strings.Builder
	textBuf    *strings.Builder
	thinkBuf   *strings.Builder
	thinkSig   string // signature for the current thinking block
	StopReason string
	Usage      Usage
	indexed    map[int]*indexedBlock
	order      []int
	source     *ReplaySource
	stopped    bool
}

type indexedBlock struct {
	block  ContentBlock
	input  strings.Builder
	closed bool
}

// NewAccumulator returns an empty Accumulator.
func NewAccumulator() *Accumulator { return &Accumulator{} }

// Add folds one event into the accumulator.
func (a *Accumulator) Add(ev StreamEvent) {
	if ev.Source != nil {
		a.source = ev.Source
	}
	if ev.Type == SEMessageStop {
		a.stopped = true
	}
	if ev.Indexed {
		a.addIndexed(ev)
		return
	}
	switch ev.Type {
	case SEMessageStart:
		a.Usage.Add(ev.Usage)
	case SETextDelta:
		if a.textBuf == nil {
			a.flushThinking()
			a.textBuf = &strings.Builder{}
		}
		a.textBuf.WriteString(ev.Text)
	case SEThinkingDelta:
		if a.thinkBuf == nil {
			a.flushText()
			a.thinkBuf = &strings.Builder{}
		}
		a.thinkBuf.WriteString(ev.Text)
	case SEThinkingSignature:
		if a.thinkBuf == nil {
			a.flushText()
			a.thinkBuf = &strings.Builder{}
		}
		a.thinkSig += ev.Text
	case SEToolUseStart:
		a.flushText()
		a.flushThinking()
		a.blocks = append(a.blocks, ContentBlock{Type: BlockToolUse, ID: ev.ToolID, Name: ev.ToolName})
		a.toolInputs = append(a.toolInputs, &strings.Builder{})
	case SEToolInputJSON:
		if n := len(a.toolInputs); n > 0 {
			a.toolInputs[n-1].WriteString(ev.Text)
		}
	case SEMessageDelta:
		if ev.StopReason != "" {
			a.StopReason = ev.StopReason
		}
		a.Usage.Add(ev.Usage)
	case SEMessageStop:
		a.flushText()
		a.flushThinking()
	}
}

func (a *Accumulator) flushText() {
	if a.textBuf != nil {
		if s := a.textBuf.String(); s != "" {
			a.blocks = append(a.blocks, TextBlock(s))
		}
		a.textBuf = nil
	}
}

func (a *Accumulator) flushThinking() {
	if a.thinkBuf != nil {
		if s := a.thinkBuf.String(); s != "" || a.thinkSig != "" {
			a.blocks = append(a.blocks, ContentBlock{
				Type:      BlockThinking,
				Thinking:  s,
				Signature: a.thinkSig,
			})
		}
		a.thinkBuf = nil
		a.thinkSig = ""
	}
}

// Message finalizes and returns the assembled assistant message. Partial tool
// inputs are validated; malformed JSON falls back to "{}".
func (a *Accumulator) Message() Message {
	a.flushText()
	a.flushThinking()
	ti := 0
	out := make([]ContentBlock, len(a.blocks))
	for i, b := range a.blocks {
		if b.Type == BlockToolUse {
			raw := strings.TrimSpace(a.toolInputs[ti].String())
			ti++
			if raw == "" || !json.Valid([]byte(raw)) {
				raw = "{}"
			}
			b.Input = json.RawMessage(raw)
		}
		out[i] = b
	}
	if len(a.order) > 0 {
		out = make([]ContentBlock, 0, len(a.order))
		for _, index := range a.order {
			ib := a.indexed[index]
			b := ib.block
			if b.Type == BlockToolUse && ib.input.Len() > 0 {
				raw := []byte(ib.input.String())
				if json.Valid(raw) {
					b.Input = append(json.RawMessage(nil), raw...)
				} else {
					b.Input = json.RawMessage("{}")
					b.IncompleteInput = string(raw)
				}
			}
			if b.Type == BlockToolUse && len(b.Input) == 0 {
				b.Input = json.RawMessage("{}")
			}
			out = append(out, b)
		}
	}
	m := Message{Role: RoleAssistant, Content: out}
	if a.source != nil || len(a.order) > 0 {
		m.Replay = &ReplayMetadata{Source: a.source, Incomplete: len(a.order) > 0 && !a.stopped}
	}
	return m
}

func (a *Accumulator) addIndexed(ev StreamEvent) {
	if a.indexed == nil {
		a.indexed = make(map[int]*indexedBlock)
	}
	ib := a.indexed[ev.BlockIndex]
	if ev.Type == SEContentBlockStart || ev.Type == SEToolUseStart {
		if ib != nil {
			return
		}
		b := ContentBlock{Type: BlockToolUse, ID: ev.ToolID, Name: ev.ToolName}
		if ev.Block != nil {
			b = *ev.Block
		}
		a.indexed[ev.BlockIndex] = &indexedBlock{block: b}
		a.order = append(a.order, ev.BlockIndex)
		return
	}
	if ib == nil {
		// Some compatible gateways omit an empty start frame. Keep their deltas
		// at the supplied index instead of discarding the assistant response.
		typ := BlockText
		switch ev.Type {
		case SEThinkingDelta, SEThinkingSignature:
			typ = BlockThinking
		case SETextDelta:
		default:
			return
		}
		ib = &indexedBlock{block: ContentBlock{Type: typ}}
		a.indexed[ev.BlockIndex] = ib
		a.order = append(a.order, ev.BlockIndex)
	}
	switch ev.Type {
	case SETextDelta:
		ib.block.Text += ev.Text
	case SEThinkingDelta:
		ib.block.Thinking += ev.Text
	case SEThinkingSignature:
		ib.block.Signature += ev.Text
	case SEToolInputJSON:
		ib.input.WriteString(ev.Text)
	case SEContentBlockStop:
		ib.closed = true
	}
}
