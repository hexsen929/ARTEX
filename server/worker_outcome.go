package server

import (
	"context"
	"errors"

	"github.com/Autumn-27/norma/harness"
)

// A checkpoint deadline after a primary run budget is not a failed model call.
// Keep the work unfinished, preserve the error, and ask the planner to inspect
// saved checkpoints instead of treating the attempt as a transport outage.
func workerOutcome(reason harness.TerminalReason, err error, completing bool) (string, modelFailureInfo) {
	failure := modelFailure(err)
	budget := reason == harness.ReasonTimeout || reason == harness.ReasonMaxTurns
	checkpoint := budget || reason == harness.ReasonSettlementRequested
	if checkpoint && errors.Is(err, context.DeadlineExceeded) {
		return "exhausted", modelFailureInfo{Kind: "settlement_timeout", Action: "review_saved_checkpoint"}
	}
	if reason == harness.ReasonSettlementRequested || completing {
		return "exhausted", failure
	}
	if err != nil {
		return "blocked", failure
	}
	if budget {
		return "exhausted", failure
	}
	return "done", failure
}
