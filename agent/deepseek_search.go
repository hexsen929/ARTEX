package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmhttp"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const deepSeekToolsURL = "https://api.deepseek.com/v1/tools/"

func deepSeekHTTPClient(proxy string) (*http.Client, error) {
	client, err := llmhttp.NewClient(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid search proxy configuration")
	}
	client.Timeout = 30 * time.Second
	return client, nil
}

func deepSeekTools(opts WebSearchOpts) []actool.CoreTool {
	if !opts.Enabled || opts.Backend != "deepseek" {
		return nil
	}
	key := strings.TrimSpace(opts.DeepSeekAPIKey)
	if key == "" {
		return nil
	}
	client, err := deepSeekHTTPClient(opts.Proxy)
	if err != nil {
		return ToolAssemblyFailure(err)
	}
	return []actool.CoreTool{
		deepSeekTool("deepseek_search", "Search the web with DeepSeek and return ranked titles, URLs, and snippets.", "search", key, client, map[string]any{
			"query": map[string]any{"type": "string", "description": "Search query"},
			"count": map[string]any{"type": "integer", "description": "Maximum results (1-10)"},
		}, "query"),
		deepSeekTool("deepseek_fetch", "Fetch a web page with DeepSeek and return its extracted content.", "fetch", key, client, map[string]any{
			"url": map[string]any{"type": "string", "description": "Page URL to fetch"},
		}, "url"),
	}
}

func DeepSeekSearchProbe(ctx context.Context, key, proxy, query string, count int) (int, error) {
	if strings.TrimSpace(key) == "" {
		return 0, fmt.Errorf("DeepSeek Search API Key is required")
	}
	client, err := deepSeekHTTPClient(proxy)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{"query": query, "count": count})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deepSeekToolsURL+"search", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("DeepSeek search returned HTTP %d", resp.StatusCode)
	}
	var result map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result); err != nil {
		return 0, fmt.Errorf("DeepSeek returned invalid JSON: %w", err)
	}
	for _, field := range []string{"results", "data", "items"} {
		var items []json.RawMessage
		if json.Unmarshal(result[field], &items) == nil && len(items) > 0 {
			return len(items), nil
		}
	}
	return 0, fmt.Errorf("DeepSeek search response did not contain results")
}

func deepSeekTool(name, description, endpoint, key string, client *http.Client, properties map[string]any, required string) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: description,
		Schema:   map[string]any{"type": "object", "properties": properties, "required": []any{required}, "additionalProperties": false},
		ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var args map[string]any
			if err := json.Unmarshal(input, &args); err != nil {
				return actool.Errorf("invalid input"), nil
			}
			for field := range args {
				if _, ok := properties[field]; !ok {
					return actool.Errorf("unexpected input field"), nil
				}
			}
			value, ok := args[required].(string)
			if !ok || strings.TrimSpace(value) == "" {
				return actool.Errorf("required input must be a non-empty string"), nil
			}
			if endpoint == "fetch" {
				u, err := url.Parse(value)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
					return actool.Errorf("page URL must be HTTP(S) without credentials"), nil
				}
			}
			if n, present := args["count"]; present {
				count, ok := n.(float64)
				if !ok || count < 1 || count > 10 || count != float64(int(count)) {
					return actool.Errorf("count must be an integer from 1 to 10"), nil
				}
			}
			body, _ := json.Marshal(args)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, deepSeekToolsURL+endpoint, strings.NewReader(string(body)))
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return actool.Errorf("DeepSeek " + endpoint + " failed: " + err.Error()), nil
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				// Providers may echo the query, URL, credentials or full request in
				// an error body. Do not persist it in tool activity/transcripts.
				return actool.Errorf(fmt.Sprintf("DeepSeek %s returned HTTP %d", endpoint, resp.StatusCode)), nil
			}
			var result json.RawMessage
			if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result); err != nil {
				return actool.Errorf("DeepSeek returned invalid JSON"), nil
			}
			return actool.Text(string(result)), nil
		},
	})
}
