package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestExtractDetailedSeparatesEmptyDraftsAndUnshareableConditions(t *testing.T) {
	stage := stageFixture()
	for _, tc := range []struct {
		name, text, outcome string
		count               int
	}{
		{"explicit empty", "[]", "empty", 0},
		{"fenced explicit empty", "```json\n[]\n```", "empty", 0},
		{"checked draft", jsonDrafts(t, draftFixture()), "drafts", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := returning(tc.text, "end_turn")
			got, err := ExtractDetailed(t.Context(), provider, "fixture-model", inputForStage(stage))
			if err != nil || got.Outcome != tc.outcome || len(got.Drafts) != tc.count || got.Drafts == nil || provider.calls != 1 {
				t.Fatalf("outcome=%q drafts=%d calls=%d err=%v", got.Outcome, len(got.Drafts), provider.calls, err)
			}
		})
	}
	stage.Facts[0].Conditions = []string{strings.Repeat("中", 61)}
	provider := returning(jsonDrafts(t, draftFixture()), "end_turn")
	got, err := ExtractDetailed(t.Context(), provider, "fixture-model", inputForStage(stage))
	if err != nil || got.Outcome != "output_rejected" || got.Drafts == nil || len(got.Drafts) != 0 || provider.calls != 1 {
		t.Fatalf("unshareable conditions were reported as empty: %+v %v", got, err)
	}
	legacy, err := Extract(t.Context(), returning(jsonDrafts(t, draftFixture()), "end_turn"), "fixture-model", inputForStage(stage))
	if err != nil || legacy == nil || len(legacy) != 0 {
		t.Fatalf("legacy complete-condition omission changed: %+v %v", legacy, err)
	}
}

func TestExtractDetailedEmptyProjectionIsNotModelNoExperience(t *testing.T) {
	stage := stageFixture()
	stage.Facts, stage.Coverage = []StageFact{}, StageCoverage{}
	provider := returning("[]", "end_turn")
	got, err := ExtractDetailed(t.Context(), provider, "fixture-model", inputForStage(stage))
	if DiagnosticCode(err) != "input_invalid" || !errors.Is(err, ErrInvalid) || got.Outcome != "" || provider.calls != 0 {
		t.Fatalf("empty projection reached model or became no-experience: %+v %v calls=%d", got, err, provider.calls)
	}
	legacy, err := Extract(t.Context(), provider, "fixture-model", inputForStage(stage))
	if err != nil || legacy == nil || len(legacy) != 0 || provider.calls != 0 {
		t.Fatalf("legacy empty-source behavior changed: %+v %v calls=%d", legacy, err, provider.calls)
	}
}

func TestExtractDetailedFixedDiagnosticCategories(t *testing.T) {
	good := jsonDrafts(t, draftFixture())
	for _, tc := range []struct {
		name, code string
		want       error
		input      func(*Input)
		response   func(*fixtureProvider)
		calls      int
	}{
		{name: "source shape", code: "input_invalid", want: ErrInvalid, input: func(in *Input) { in.Content = `{}` }},
		{name: "source secret", code: "input_sensitive", want: ErrSensitive, input: func(in *Input) { in.Content = `password=fixture-secret` }},
		{name: "response null", code: "output_invalid", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning("null", "end_turn") }, calls: 1},
		{name: "response object", code: "output_invalid", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning(`{"entries":[]}`, "end_turn") }, calls: 1},
		{name: "missing fields", code: "output_invalid", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning(`[{"title":"fixture"}]`, "end_turn") }, calls: 1},
		{name: "response role", code: "output_invalid", want: ErrInvalid, response: func(p *fixtureProvider) {
			p.complete = func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				message := textResponse(good)
				message.Role = llm.RoleUser
				return message, "end_turn", llm.Usage{}, nil
			}
		}, calls: 1},
		{name: "length", code: "output_truncated", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning(good, "length") }, calls: 1},
		{name: "max tokens", code: "output_truncated", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning(good, "max_tokens") }, calls: 1},
		{name: "tool response", code: "output_invalid", want: ErrInvalid, response: func(p *fixtureProvider) { *p = *returning(good, "tool_calls") }, calls: 1},
		{name: "output secret", code: "output_sensitive", want: ErrSensitive, response: func(p *fixtureProvider) {
			draft := draftFixture()
			draft.Content = "password=fixture-secret"
			*p = *returning(jsonDrafts(t, draft), "end_turn")
		}, calls: 1},
		{name: "invented evidence", code: "output_rejected", want: ErrInvalid, response: func(p *fixtureProvider) {
			draft := draftFixture()
			draft.EvidenceIDs = []string{"fact-not-present"}
			*p = *returning(jsonDrafts(t, draft), "end_turn")
		}, calls: 1},
		{name: "unsupported technical condition", code: "output_rejected", want: ErrInvalid, response: func(p *fixtureProvider) {
			draft := draftFixture()
			draft.Content = "Windows v99.0 的部署观察。"
			*p = *returning(jsonDrafts(t, draft), "end_turn")
		}, calls: 1},
		{name: "provider error", code: "provider_error", want: ErrUnavailable, response: func(p *fixtureProvider) {
			p.complete = func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return llm.Message{}, "", llm.Usage{}, errors.New("fixture-private-provider-body-and-key")
			}
		}, calls: 1},
		{name: "provider timeout", code: "timeout", want: context.DeadlineExceeded, response: func(p *fixtureProvider) {
			p.complete = func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return llm.Message{}, "", llm.Usage{}, context.DeadlineExceeded
			}
		}, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, provider := sourceFixture(), returning(good, "end_turn")
			if tc.input != nil {
				tc.input(&input)
			}
			if tc.response != nil {
				tc.response(provider)
			}
			got, err := ExtractDetailed(t.Context(), provider, "fixture-model", input)
			if DiagnosticCode(err) != tc.code || !errors.Is(err, tc.want) || provider.calls != tc.calls || len(got.Drafts) != 0 || got.Outcome != "" {
				t.Fatalf("code=%q want=%q err=%v calls=%d outcome=%q", DiagnosticCode(err), tc.code, err, provider.calls, got.Outcome)
			}
			if strings.Contains(fmt.Sprint(err), "fixture-private-provider-body-and-key") {
				t.Fatal("provider error text escaped body-free diagnostics")
			}
		})
	}
}

func TestExtractDetailedPreservesWholeBatchRejection(t *testing.T) {
	good, bad := draftFixture(), draftFixture()
	bad.EvidenceIDs = []string{"invented"}
	provider := returning(jsonDrafts(t, good, bad), "end_turn")
	got, err := ExtractDetailed(t.Context(), provider, "fixture-model", sourceFixture())
	if DiagnosticCode(err) != "output_rejected" || got.Drafts != nil || provider.calls != 1 {
		t.Fatalf("bad second draft partially activated the batch: %+v %v", got, err)
	}
}

func TestDiagnosticCodeCancellationAndUnclassifiedErrorsRemainBodyFree(t *testing.T) {
	if DiagnosticCode(nil) != "" || DiagnosticCode(context.Canceled) != "" || DiagnosticCode(errors.New("fixture-private-body")) != "" {
		t.Fatal("nil/cancelled/unknown errors acquired invented categories")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	provider := returning("[]", "end_turn")
	if _, err := ExtractDetailed(ctx, provider, "fixture-model", sourceFixture()); !errors.Is(err, context.Canceled) || provider.calls != 0 {
		t.Fatalf("cancelled extraction reached provider: %v calls=%d", err, provider.calls)
	}
	if DiagnosticCode(fmt.Errorf("fixture wrap: %w", context.DeadlineExceeded)) != "timeout" {
		t.Fatal("wrapped deadline lost its fixed category")
	}
}

func TestAuthorizationPlaceholderRequiresCompleteExistingLiteral(t *testing.T) {
	for _, text := range []string{
		"Authorization: Bearer TOKEN", "Authorization: Basic TOKEN", "Proxy-Authorization: Bearer API_KEY",
		"authorization: bearer PASSWORD", "Authorization: Bearer [REDACTED]", "Authorization: Basic <MASKED>",
		"`Authorization: Bearer TOKEN`", `{"summary":"Authorization: Bearer TOKEN"}`,
	} {
		if clean, err := SanitizeSource(text); err != nil || clean != text {
			t.Errorf("literal header placeholder rejected: %q err=%v", text, err)
		}
	}
	for _, text := range []string{
		"Authorization: Bearer fixture-real-token", "Authorization: Basic dXNlcjpmaXh0dXJlLXNlY3JldA==",
		"Authorization: Bearer TOKEN-suffix", "Authorization: Bearer TOKEN_real", "Authorization: Bearer [TOKEN]-real",
		"Authorization: Bearer TOKEN real-secret", "Authorization: Bearer TOKEN]", "Authorization: Bearer token",
		"password=Bearer TOKEN", "api_key=Basic TOKEN", "Authorization: Bearer TOKEN\npassword=fixture-secret",
	} {
		if _, err := SanitizeSource(text); !errors.Is(err, ErrSensitive) {
			t.Errorf("real/ambiguous credential was allowed: %q err=%v", text, err)
		}
	}
	stage := stageFixture()
	stage.Facts[0].Summary = "Authorization: Bearer TOKEN"
	provider := returning("[]", "end_turn")
	got, err := ExtractDetailed(t.Context(), provider, "fixture-model", inputForStage(stage))
	if err != nil || got.Outcome != "empty" || provider.calls != 1 {
		t.Fatalf("literal placeholder in a native fact did not reach model: %+v %v", got, err)
	}
}

func TestBareTargetAliasesDoNotMatchMethodologicalWordSubstrings(t *testing.T) {
	for _, tc := range []struct {
		target, text string
		want         bool
	}{
		{"test", "Use testing baselines and retain conditions.", false},
		{"host", "Check hosting compatibility before using the tool.", false},
		{"test", "test", true}, {"test", "TEST", true}, {"host", "host", true},
		{"test", "Use test as the target.", true}, {"test", `\u0074est`, true},
		{"target.invalid", "prefix-target.invalid-suffix", true},
		{"192.0.2.15", "192.0.2.150", true}, {"/api/v1", "/api/v10", true},
		{"12345", "prefix12345suffix", true},
	} {
		if got := containsTargetDetails([]string{tc.text}, []StageFact{{Target: tc.target}}); got != tc.want {
			t.Errorf("target=%q text=%q: got=%v want=%v", tc.target, tc.text, got, tc.want)
		}
	}
}
