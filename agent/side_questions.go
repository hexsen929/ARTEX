package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/agentcore"
)

func attachSideCapture(ctx context.Context, opts *agentcore.Options) context.Context {
	p, ok := sideCaptureParent(RunInfoFrom(ctx))
	if !ok {
		return ctx
	}
	ctx, opts.Deps = sidequestion.Attach(ctx, p, opts.Deps, opts.Provider)
	return ctx
}

// A task-associated conversation still belongs to its conversation's /btw
// history. Keep task attribution in RunInfo for tools, proxying and auditing,
// rather than copying that context into the exclusive checkpoint parent fields.
func sideCaptureParent(ri RunInfo) (sidequestion.Parent, bool) {
	if strings.HasPrefix(ri.SessionID, "conv-") {
		idText := strings.TrimPrefix(ri.SessionID, "conv-")
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
			// A malformed conversation identity must not silently become a task
			// checkpoint merely because task context was inherited.
			return sidequestion.Parent{}, false
		}
		return sidequestion.Parent{ConversationID: id}, true
	}
	p := sidequestion.Parent{TaskID: ri.TaskID, ExplorationID: ri.ExplorationID, IntentID: ri.IntentID}
	if err := p.Validate(); err != nil {
		return sidequestion.Parent{}, false
	}
	return p, true
}
