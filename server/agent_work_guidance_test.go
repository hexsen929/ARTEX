package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestAgentWorkGuidanceMetadataForKnownAndCustomRoles(t *testing.T) {
	version, guidance := agentWorkGuidanceMetadata("worker")
	if version != agent.AgentWorkGuidanceVersion || guidance != agent.AgentWorkGuidance("worker") || guidance == "" {
		t.Fatal("built-in role guidance metadata does not match runtime")
	}
	version, guidance = agentWorkGuidanceMetadata("custom_fixture_role")
	if version != "" || guidance != "" {
		t.Fatal("unknown custom role was assigned built-in guidance")
	}
}

func TestAgentWorkGuidanceAPIPreservesCustomPromptAndVersions(t *testing.T) {
	pg := openIntranetTestDB(t)
	s := &Server{m: &Manager{pg: pg}}
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatal("worker fixture unavailable", err)
	}
	old, err := pg.CurrentPrompt(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.SavePrompt(worker.ID, old, "restore fixture", "test") })
	const custom = "Custom delivery body {{.WorkerName}}; preserve the original workflow."
	if _, err := pg.SavePrompt(worker.ID, custom, "custom fixture", "test"); err != nil {
		t.Fatal(err)
	}
	before, err := pg.ListPromptVersions(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := func(handler http.HandlerFunc, method, key, body string) map[string]json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, "/api/agents/"+key, strings.NewReader(body))
		r.SetPathValue("key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("agent management request returned %d: %s", w.Code, w.Body.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	text := func(result map[string]json.RawMessage, key string) string {
		t.Helper()
		var value string
		if err := json.Unmarshal(result[key], &value); err != nil {
			t.Fatalf("missing or invalid field %s: %v", key, err)
		}
		return value
	}
	checkGuidance := func(result map[string]json.RawMessage) {
		t.Helper()
		if text(result, "work_guidance_version") != agent.AgentWorkGuidanceVersion || text(result, "work_guidance") != agent.AgentWorkGuidance("worker") {
			t.Fatal("management projection disagrees with runtime guidance")
		}
	}

	detail := request(s.pgGetAgent, "GET", "worker", "")
	checkGuidance(detail)
	if text(detail, "prompt") != custom {
		t.Fatal("detail replaced the editable body with runtime guidance")
	}
	preview := request(s.pgPreviewPrompt, "POST", "worker", `{"template":"CUSTOM {{.WorkerName}}","sample":{"WorkerName":"fixture-worker"}}`)
	checkGuidance(preview)
	if text(preview, "rendered") != "CUSTOM fixture-worker" {
		t.Fatal("preview did not keep editable body and runtime guidance separate")
	}
	bad := request(s.pgPreviewPrompt, "POST", "worker", `{"template":"{{.UnknownGuidanceFixture}}"}`)
	checkGuidance(bad)
	if text(bad, "rendered") != "" || text(bad, "error") == "" {
		t.Fatal("invalid template preview lost its error or guidance metadata")
	}
	current, err := pg.CurrentPrompt(worker.ID)
	if err != nil || current != custom {
		t.Fatal("read/preview changed the stored custom prompt", err)
	}
	after, err := pg.ListPromptVersions(worker.ID)
	if err != nil || len(before) != len(after) {
		t.Fatal("read/preview created a prompt version", err)
	}

	const savedBody = "Updated custom body; retain this exact text."
	request(s.pgSavePrompt, "PUT", "worker", `{"template":"`+savedBody+`","note":"fixture"}`)
	current, err = pg.CurrentPrompt(worker.ID)
	if err != nil || current != savedBody || strings.Contains(current, agent.AgentWorkGuidanceVersion) {
		t.Fatal("save injected runtime guidance into editable prompt history", err)
	}
	after, err = pg.ListPromptVersions(worker.ID)
	if err != nil || len(after) != len(before)+1 {
		t.Fatal("save did not create exactly one editable prompt version", err)
	}

	key := "guidance_fixture_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	customAgent, err := pg.CreateAgent(key, "Guidance fixture", "custom agent retains its behavior")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteAgent(customAgent.Key) })
	unknown := request(s.pgGetAgent, "GET", key, "")
	if text(unknown, "work_guidance") != "" || text(unknown, "work_guidance_version") != "" {
		t.Fatal("custom agent received built-in runtime guidance")
	}
}
