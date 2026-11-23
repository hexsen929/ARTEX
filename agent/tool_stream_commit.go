package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

const maxToolTurnBytes = 16 << 20
const maxToolTurnEvents = 65536

var errToolTurnTooLarge = errors.New("tool_response_buffer_limit: 工具响应超过接收上限，本轮未执行工具")

// The SDK finalizes and may execute an unfinished tool block even when the
// provider subsequently fails. Commit tool-bearing turns only after a clean
// model response. Text before the first tool remains live; failed tool fragments
// never reach the executor, transcript or durable execution checkpoint.
func committedToolStream(ctx context.Context, events iter.Seq2[llm.StreamEvent, error]) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		var buffered []llm.StreamEvent
		buffering := false
		size := 0
		stop := ""
		var input strings.Builder
		seen := map[string]bool{}
		valid := true
		finishInput := func() {
			if !buffering {
				return
			}
			raw := strings.TrimSpace(input.String())
			if raw != "" && !json.Valid([]byte(raw)) {
				valid = false
			}
			input.Reset()
		}
		for event, err := range events {
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if ctx.Err() != nil {
				yield(llm.StreamEvent{}, ctx.Err())
				return
			}
			if event.Type == llm.SEToolUseStart {
				finishInput()
				buffering = true
				if event.ToolID == "" || event.ToolName == "" || seen[event.ToolID] {
					valid = false
				}
				seen[event.ToolID] = true
			}
			if !buffering {
				if !yield(event, nil) {
					return
				}
				continue
			}
			if event.Type == llm.SEToolInputJSON {
				input.WriteString(event.Text)
			}
			if event.Type == llm.SEMessageDelta && event.StopReason != "" {
				stop = event.StopReason
			}
			size += len(event.Text) + len(event.ToolID) + len(event.ToolName) + 128
			if size > maxToolTurnBytes || len(buffered) >= maxToolTurnEvents {
				yield(llm.StreamEvent{}, errToolTurnTooLarge)
				return
			}
			buffered = append(buffered, event)
		}
		if !buffering {
			return
		}
		finishInput()
		if ctx.Err() != nil {
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		if !valid || (stop != "tool_use" && stop != "end_turn" && stop != "stop") {
			yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
			return
		}
		for _, event := range buffered {
			if !yield(event, nil) {
				return
			}
		}
	}
}
