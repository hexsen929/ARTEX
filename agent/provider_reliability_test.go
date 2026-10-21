package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/llmguard"
)

func TestRelayClassificationAndTotalHTTPBudget(t *testing.T) {
	for _, tc := range []struct {
		body         string
		status, want int
	}{
		{`{"error":{"code":"model_not_found","message":"No available channel"}}`, 503, 404},
		{`overloaded`, 503, 503}, {`insufficient_quota`, 503, 503}, {`insufficient_quota`, 429, 402},
	} {
		calls := 0
		transport := quotaAwareTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})}
		ctx := llmguard.WithBudget(context.Background(), 2)
		for range 2 {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid/v1/chat/completions", nil)
			resp, e := transport.RoundTrip(r)
			if e != nil || resp.StatusCode != tc.want {
				t.Fatalf("status=%v error=%v", resp, e)
			}
			resp.Body.Close()
		}
		r, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid/v1/chat/completions", nil)
		_, e := transport.RoundTrip(r)
		if !errors.Is(e, llmguard.ErrRetryBudget) || calls != 2 {
			t.Fatalf("attempts %d err %v", calls, e)
		}
	}
}
