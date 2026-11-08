package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestExtractDetailedTruncationRecoveryRemainsBounded(t *testing.T) {
	for _, tc := range []struct {
		truncations, tokens int
		duration            time.Duration
	}{{-1, 3000, time.Minute}, {0, 3000, time.Minute}, {1, 6000, 90 * time.Second}, {2, 6000, 90 * time.Second}, {99, 6000, 90 * time.Second}} {
		provider := &fixtureProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			deadline, ok := ctx.Deadline()
			if req.MaxTokens != tc.tokens || !ok || time.Until(deadline) > tc.duration || time.Until(deadline) < tc.duration-time.Second {
				t.Fatal("recovery request did not retain bounded output and time allowances")
			}
			if len(req.Tools) != 0 || req.Thinking != "disabled" || len(req.Messages) != 1 {
				t.Fatal("recovery acquired tools, reasoning instructions or history")
			}
			return textResponse(jsonDrafts(t, draftFixture())), "end_turn", llm.Usage{}, nil
		}}
		got, err := ExtractAfterTruncations(t.Context(), provider, "fixture", sourceFixture(), tc.truncations)
		if err != nil || got.Outcome != "drafts" || len(got.Drafts) != 1 || provider.calls != 1 {
			t.Fatalf("bounded recovery failed: outcome=%s calls=%d err=%v", got.Outcome, provider.calls, err)
		}
	}
	// A larger provider allowance never permits a larger stored note or a
	// partial JSON response, even if it contains an apparently complete record.
	oversized := draftFixture()
	oversized.Content = strings.Repeat("x", MaxDraftContentBytes+1)
	for _, tc := range []struct{ body, stop, code string }{
		{jsonDrafts(t, oversized), "end_turn", "output_invalid"},
		{jsonDrafts(t, draftFixture()), "max_tokens", "output_truncated"},
	} {
		provider := returning(tc.body, tc.stop)
		got, err := ExtractAfterTruncations(t.Context(), provider, "fixture", sourceFixture(), 2)
		if DiagnosticCode(err) != tc.code || len(got.Drafts) != 0 || provider.calls != 1 {
			t.Fatalf("recovery weakened validation or retried internally: code=%s calls=%d", DiagnosticCode(err), provider.calls)
		}
	}
}
