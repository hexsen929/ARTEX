package agent

import (
	"context"
	"iter"
	"log"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/memcontext"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

// MemoryRecallResult is one bounded retrieval, never a session-fixed prompt.
// The host owns scope/role/source/expiry filtering and the model-context budget.
// Revalidate checks settings, entry revisions and source validity at each actual
// HTTP attempt; it must not hold locks while the model processes the request.
type MemoryRecallResult struct {
	Block      string
	References []memcontext.Reference
	Revalidate func(context.Context) error
}

// MemoryRecall is wired by the server. A nil hook preserves existing behavior.
// The supplied identity comes from the host context, never model-written IDs.
// Retrieval errors are optional-memory failures, not authorization failures.
var MemoryRecall func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error)

func memoryRoleAllowed(role string) bool {
	return role != "" && role != db.CredentialAgentKey && role != "asset_mapper" && role != "worker.intranet" && role != "memory"
}

// attachMemory wraps only the harness's primary model calls. Auxiliary title,
// compaction and extraction calls still use the original provider, so transient
// memory never becomes direct compression input or recursively calls itself.
func attachMemory(ctx context.Context, opts *agentcore.Options, role string) context.Context {
	if opts == nil || opts.Provider == nil || MemoryRecall == nil || !memoryRoleAllowed(role) {
		return ctx
	}
	identity := RunInfoFrom(ctx)
	if identity.TaskID <= 0 && strings.TrimSpace(identity.SessionID) == "" {
		return ctx
	}
	recall := MemoryRecall
	budget := 1600 // unknown/no compaction window: conservative 32k baseline
	if opts.Compaction != nil && opts.Compaction.ContextWindow > 0 {
		budget = min(2000, opts.Compaction.ContextWindow/20)
	}
	stream, complete := opts.Deps.CallModel, opts.Deps.CallModelSync
	if stream == nil {
		stream = opts.Provider.Stream
	}
	if complete == nil {
		complete = opts.Provider.Complete
	}
	opts.Deps.CallModel = func(callCtx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			memoryCtx, request, release := recallMemory(memcontext.WithByteBudget(callCtx, budget), identity, role, req, recall)
			defer release()
			for event, err := range stream(memoryCtx, request) {
				if stale := memcontext.ErrorFrom(memoryCtx); stale != nil {
					yield(llm.StreamEvent{}, stale)
					return
				}
				if !yield(event, err) {
					return
				}
			}
			if stale := memcontext.ErrorFrom(memoryCtx); stale != nil {
				yield(llm.StreamEvent{}, stale)
			}
		}
	}
	opts.Deps.CallModelSync = func(callCtx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		memoryCtx, request, release := recallMemory(memcontext.WithByteBudget(callCtx, budget), identity, role, req, recall)
		defer release()
		msg, stop, usage, err := complete(memoryCtx, request)
		if stale := memcontext.ErrorFrom(memoryCtx); stale != nil {
			return llm.Message{}, "", usage, stale
		}
		return msg, stop, usage, err
	}
	return ctx
}

func recallMemory(ctx context.Context, identity RunInfo, role string, req llm.CompletionRequest, recall func(context.Context, RunInfo, string, llm.CompletionRequest) (MemoryRecallResult, error)) (context.Context, llm.CompletionRequest, func()) {
	result, err := recall(ctx, identity, role, req)
	if err != nil {
		// No provider message, task text or database error is copied to logs.
		log.Printf("[memory] recall unavailable task=%d session=%q role=%q; continuing without memory", identity.TaskID, identity.SessionID, role)
		return ctx, req, func() {}
	}
	if strings.TrimSpace(result.Block) == "" {
		return ctx, req, func() {}
	}
	// Defense in depth: reject an unbounded or unverifiable hook result rather
	// than silently truncating evidence or admitting dependencies without checks.
	valid := len(result.Block) <= 8192 && len(result.References) > 0 && len(result.References) <= 10 && result.Revalidate != nil
	seen := make(map[int64]bool, len(result.References))
	for _, ref := range result.References {
		if ref.ID <= 0 || ref.Version <= 0 || seen[ref.ID] {
			valid = false
		}
		seen[ref.ID] = true
	}
	if !valid {
		log.Printf("[memory] recall rejected invalid metadata task=%d role=%q; continuing without memory", identity.TaskID, role)
		return ctx, req, func() {}
	}
	block := memcontext.FormatBlock(result.Block, result.References)
	if len(block) > memcontext.ByteBudget(ctx) {
		log.Printf("[memory] recall exceeds full-block budget task=%d role=%q; continuing without memory", identity.TaskID, role)
		return ctx, req, func() {}
	}
	request := req
	request.System = append(append([]string(nil), req.System...), block)
	memoryCtx, cancel := memcontext.WithRecall(ctx, result.References, result.Revalidate)
	return memoryCtx, request, cancel
}
