package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embed uses only the explicitly selected endpoint. Redirects are refused so
// neither credentials nor document contents follow an unselected destination.
func Embed(ctx context.Context, c Config, texts []string) ([][]float32, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !c.Enabled || len(texts) == 0 || len(texts) > 16 {
		return nil, ErrInput
	}
	for _, s := range texts {
		if !ValidText(s, 8000) {
			return nil, ErrInput
		}
	}
	body, _ := json.Marshal(map[string]any{"model": c.Model, "input": texts, "dimensions": c.Dimension})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, ErrInput
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("embedding endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("embedding endpoint rejected request")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("embedding response too large")
	}
	var wire struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Data) != len(texts) {
		return nil, errors.New("invalid embedding response")
	}
	out := make([][]float32, len(texts))
	for _, entry := range wire.Data {
		if entry.Index < 0 || entry.Index >= len(out) || out[entry.Index] != nil || !ValidVector(entry.Embedding, c.Dimension) {
			return nil, errors.New("invalid embedding dimensions or indices")
		}
		out[entry.Index] = entry.Embedding
	}
	return out, nil
}
