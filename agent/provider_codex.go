package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

const codexInstructions = "You are Codex, a command-line coding agent."

// Model files contain literal text, separate from the templated Agent Prompt.
// Read them when constructing the provider; the file editor invalidates cached
// providers after saving, so subsequent calls use the newly saved content.
func readModelInstructionsFile(path string) (string, error) {
	return config.ReadInstructionsTextFile(path)
}

type configurationErrorProvider struct{ err error }

// ErrorProvider keeps a selected but invalid profile selected, returning its
// local error from both completion paths without making a network request.
func ErrorProvider(err error) llm.Provider {
	if err == nil {
		err = fmt.Errorf("LLM configuration is invalid")
	}
	return configurationErrorProvider{err: err}
}

func (p configurationErrorProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, p.err
}

func (p configurationErrorProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, p.err) }
}

// Codex relays can require streaming even for the host's internal summary calls.
// Buffer the same streaming provider for Complete, retaining tools and usage.
type codexResponsesProvider struct{ llm.Provider }

func (p codexResponsesProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	if err := ctx.Err(); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	if acc.StopReason == "" {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("openai-responses: Codex stream ended before response.completed")
	}
	msg := acc.Message()
	stop := acc.StopReason
	if stop == "end_turn" && len(msg.ToolUses()) > 0 {
		stop = "tool_use"
	}
	return msg, stop, acc.Usage, nil
}

// adaptCodexResponsesRequest is opt-in via the Codex preset. Keep Norma's tool
// conversion, auth, retries, and SSE parsing, and adapt only the outgoing wire
// request. RawMessage preserves tool schemas and function arguments exactly.
func adaptCodexResponsesRequest(req *http.Request, modelInstructions, developerInstructions string, layered bool) error {
	if req.Body == nil {
		return fmt.Errorf("openai-responses: missing Codex request body")
	}
	original := req.Body
	defer original.Close()
	source := original
	if req.GetBody != nil {
		var err error
		source, err = req.GetBody()
		if err != nil {
			return fmt.Errorf("openai-responses: copy Codex request: %w", err)
		}
		defer source.Close()
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return fmt.Errorf("openai-responses: read Codex request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return fmt.Errorf("openai-responses: invalid Codex request JSON")
	}
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(body["input"], &input); err != nil {
		return fmt.Errorf("openai-responses: invalid Codex input: %w", err)
	}
	for _, item := range input {
		var role, text string
		_ = json.Unmarshal(item["role"], &role)
		if role == "" {
			continue // typed function_call / function_call_output items stay intact
		}
		if err := json.Unmarshal(item["content"], &text); err != nil {
			continue // already typed content stays intact
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		item["type"] = json.RawMessage(`"message"`)
		item["content"], _ = json.Marshal([]map[string]string{{"type": contentType, "text": text}})
	}
	// Codex 0.162.1 sends base instructions as their own developer message.
	// Config-level developer_instructions and the host Agent Prompt remain
	// separate messages; a model file never replaces the Agent Prompt.
	var instructions string
	_ = json.Unmarshal(body["instructions"], &instructions)
	if modelInstructions == "" {
		modelInstructions = codexInstructions
	}
	if layered {
		prefix := make([]map[string]json.RawMessage, 0, 3)
		for _, text := range []string{modelInstructions, developerInstructions, instructions} {
			if text == "" {
				continue
			}
			content, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
			prefix = append(prefix, map[string]json.RawMessage{
				"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"developer"`), "content": content,
			})
		}
		input = append(prefix, input...)
		delete(body, "instructions")
	} else {
		// Existing profiles retain the gateway compatibility format until a
		// model file or a Codex config is selected explicitly.
		if instructions == "" {
			instructions = modelInstructions
		} else {
			instructions = modelInstructions + "\n\n" + instructions
		}
		body["instructions"], _ = json.Marshal(instructions)
	}
	body["input"], err = json.Marshal(input)
	if err != nil {
		return fmt.Errorf("openai-responses: encode Codex input: %w", err)
	}
	body["stream"] = json.RawMessage(`true`)
	body["store"] = json.RawMessage(`false`)
	body["tool_choice"] = json.RawMessage(`"auto"`)
	body["parallel_tool_calls"] = json.RawMessage(`false`)
	if _, ok := body["tools"]; !ok {
		body["tools"] = json.RawMessage(`[]`)
	}
	body["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	// The Codex backend does not accept the normal Responses output cap or
	// sampling parameters (also removed by new-api's Codex channel adapter).
	for _, key := range []string{"max_output_tokens", "temperature", "frequency_penalty", "presence_penalty"} {
		delete(body, key)
	}
	sid := transcript.SessionIDFrom(req.Context())
	if sid == "" {
		sid = uuid.NewString()
	} else if _, err := uuid.Parse(sid); err != nil {
		sid = uuid.NewSHA1(uuid.NameSpaceURL, []byte("artex:"+sid)).String()
	}
	body["prompt_cache_key"], _ = json.Marshal(sid)
	raw, err = json.Marshal(body)
	if err != nil {
		return fmt.Errorf("openai-responses: encode Codex request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	req.Header.Del("Content-Length")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("version", "0.162.1")
	req.Header.Set("session-id", sid)
	return nil
}
