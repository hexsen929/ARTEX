package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func credentialToolCall(t *testing.T, ctx context.Context, tool actool.CoreTool, input any, wantError bool) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(ctx, raw, nil)
	if err != nil || result.IsError != wantError {
		t.Fatalf("%s returned error=%v is_error=%v", tool.Name(), err, result.IsError)
	}
	return result.Flatten()
}

func TestCredentialToolSchemasMatchVerificationFlow(t *testing.T) {
	s := &Server{credStore: db.NewCredentialStore(nil, []byte("fixture-key"))}
	tools := s.credentialTools()
	if len(tools) != 4 {
		t.Fatalf("credential tool count=%d, want 4", len(tools))
	}
	add := s.addCredentialTool()
	if strings.Contains(add.Description(), "verify=true") || !strings.Contains(add.Description(), "record_credential_verification") {
		t.Fatal("credential registration advertises an unsupported verification parameter")
	}
	properties := add.InputSchema()["properties"].(map[string]any)
	for _, key := range []string{"target", "source_url", "domain"} {
		if properties[key] == nil {
			t.Fatalf("missing metadata schema property %s", key)
		}
	}
	for _, key := range []string{"verify", "verified", "task_id", "source_intent_id", "source_exploration_id"} {
		if properties[key] != nil {
			t.Fatalf("client-controlled provenance/verification property %s", key)
		}
	}
	verification := s.recordCredentialVerificationTool()
	if verification.Name() != "record_credential_verification" || verification.IsReadOnly(nil) {
		t.Fatal("verification recorder must be an explicit write tool")
	}
	if !strings.Contains(verification.Description(), "不会自动访问目标") {
		t.Fatal("verification recorder must state it records existing results only")
	}
}

func TestCredentialToolStrictInputDoesNotEchoSecret(t *testing.T) {
	s := &Server{}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 1})
	const secret = "fixture-secret-never-echo-this"
	for _, input := range []any{
		map[string]any{"cred_type": secret, "secret": secret, "source": "fixture"},
		map[string]any{"cred_type": "password", "secret": secret, "source": "fixture", "verify": true},
		map[string]any{"cred_type": "password", "secret": secret, "source": "fixture", "task_id": 999},
		map[string]any{"cred_type": "password", "secret": secret, "source": "fixture", "source_intent_id": 999},
		map[string]any{"cred_type": "password", "secret": secret, "source": "fixture", "target": "https://user:" + secret + "@fixture.invalid"},
		map[string]any{"cred_type": "password", "secret": secret, "source": "fixture", "source_url": "javascript:" + secret},
		map[string]any{"cred_type": "password", "secret": strings.Repeat("x", (64<<10)+1), "source": "fixture"},
	} {
		output := credentialToolCall(t, ctx, s.addCredentialTool(), input, true)
		if strings.Contains(output, secret) {
			t.Fatal("credential validation error echoed sensitive input")
		}
	}
	for _, raw := range []string{"null", "[]", "{} {}", `{"secret":`, strings.Repeat(" ", 3) + `{"unknown":"` + secret + `"}`} {
		result, err := s.addCredentialTool().Call(ctx, json.RawMessage(raw), nil)
		if err != nil || !result.IsError || strings.Contains(result.Flatten(), secret) {
			t.Fatal("malformed input was accepted or echoed")
		}
	}
	if decodeCredentialToolInput(json.RawMessage(`{"a":1}`), &struct{}{}) || decodeCredentialToolInput(json.RawMessage("{"+strings.Repeat(" ", 128<<10)+"}"), &struct{}{}) {
		t.Fatal("unknown fields or oversized objects should be rejected")
	}
	credentialToolCall(t, context.Background(), s.addCredentialTool(), map[string]any{"cred_type": "password", "secret": secret, "source": "fixture"}, true)
	credentialToolCall(t, context.Background(), s.listCredentialsTool(), map[string]any{}, true)
	credentialToolCall(t, ctx, s.listCredentialsTool(), map[string]any{"secret": secret}, true)
	credentialToolCall(t, ctx, s.recordCredentialVerificationTool(), map[string]any{"credential_id": 1, "status": "verified"}, true)
	credentialToolCall(t, ctx, s.recordCredentialVerificationTool(), map[string]any{"credential_id": 1, "status": "failed", "method": "fixture", "evidence": "fixture", "by": "forged"}, true)
}

func TestCredentialToolsPreserveTaskProvenanceAndVerificationHistory(t *testing.T) {
	pg := openIntranetTestDB(t)
	store := db.NewCredentialStore(pg, []byte("fixture-credential-tools-key"))
	s := &Server{credStore: store}
	task, err := pg.CreateTask("credential tool provenance fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	intentID, err := pg.Exploration(task.ExplorationID).AddIntent(map[string]any{"title": "fixture credential collection"}, 0, nil, "worker")
	if err != nil {
		t.Fatal(err)
	}
	ri := agent.RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intentID}
	ctx := agent.WithRunInfo(context.Background(), ri)
	const secret = "fixture-complete-secret-value"
	result := credentialToolCall(t, ctx, s.addCredentialTool(), map[string]any{
		"cred_type": "password", "secret": secret, "username": "fixture-user", "source": "fixture stored response",
		"target": "https://fixture.invalid/account?token=drop#drop", "source_url": "https://fixture.invalid/evidence?token=drop#drop",
	}, false)
	var created struct {
		CredentialID int64 `json:"credential_id"`
	}
	if err := json.Unmarshal([]byte(result), &created); err != nil || created.CredentialID <= 0 || strings.Contains(result, secret) {
		t.Fatal("registration did not return only a credential ID")
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), created.CredentialID) })
	rec, err := store.Get(ctx, created.CredentialID)
	if err != nil || rec == nil {
		t.Fatal("credential not persisted", err)
	}
	if rec.TaskID != task.ID || rec.SourceIntentID != intentID || rec.SourceExplorationID != task.ExplorationID || rec.Domain != "" || rec.Target != "https://fixture.invalid/account" || rec.SourceURL != "https://fixture.invalid/evidence" || rec.Verified || rec.VerificationStatus != "unverified" {
		t.Fatal("registration lost task provenance, mixed website/AD domains, or forged verification")
	}
	listed := credentialToolCall(t, ctx, s.listCredentialsTool(), map[string]any{}, false)
	if strings.Contains(listed, secret) || strings.Contains(listed, `"secret":`) || !strings.Contains(listed, `"verification_status":"unverified"`) {
		t.Fatal("credential list leaked plaintext or omitted verification state")
	}
	otherTask, err := pg.CreateTask("credential isolation fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(otherTask.ID) })
	otherCtx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: otherTask.ID, ExplorationID: otherTask.ExplorationID})
	credentialToolCall(t, otherCtx, s.recordCredentialVerificationTool(), map[string]any{"credential_id": created.CredentialID, "status": "verified", "method": "fixture response", "evidence": "fixture proof #1"}, true)
	rec, err = store.Get(ctx, created.CredentialID)
	if err != nil || rec.Verified || len(rec.VerificationHistory) != 0 {
		t.Fatal("cross-task tool call changed credential state")
	}
	for _, status := range []string{"verified", "failed", "unverified"} {
		output := credentialToolCall(t, ctx, s.recordCredentialVerificationTool(), map[string]any{
			"credential_id": created.CredentialID, "status": status, "method": "observed fixture result", "evidence": "fixture proof #1",
		}, false)
		if strings.Contains(output, secret) || strings.Contains(output, "fixture proof #1") {
			t.Fatal("verification output contains secret/evidence rather than the status acknowledgement")
		}
	}
	rec, err = store.Get(ctx, created.CredentialID)
	if err != nil || rec.Verified || rec.VerificationStatus != "unverified" || rec.VerificationAt != nil || rec.VerificationMethod != "" || rec.VerificationEvidence != "" || len(rec.VerificationHistory) != 3 {
		t.Fatal("reset lost history or retained an asserted verification")
	}
	wantActor := fmt.Sprintf("agent:task-%d:exploration-%d:intent-%d", ri.TaskID, ri.ExplorationID, ri.IntentID)
	for index, status := range []string{"verified", "failed", "unverified"} {
		event := rec.VerificationHistory[index]
		if event.Status != status || event.By != wantActor || event.At.IsZero() {
			t.Fatal("verification history lost server-side actor or transition order")
		}
	}
	if _, err := pg.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	credentialToolCall(t, ctx, s.recordCredentialVerificationTool(), map[string]any{"credential_id": created.CredentialID, "status": "verified", "method": "fixture", "evidence": "fixture"}, true)
	if _, err := pg.Exec(`UPDATE tasks SET archived_at=NULL WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialToolMetadataUpgradePreservesCustomization(t *testing.T) {
	pg := openIntranetTestDB(t)
	s := &Server{m: &Manager{pg: pg}, credStore: db.NewCredentialStore(pg, []byte("fixture-tool-schema-key"))}
	key := "add_credential"
	old, err := pg.GetTool(key)
	if err != nil {
		t.Fatal(err)
	}
	if old == nil {
		t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, key) })
	} else {
		t.Cleanup(func() {
			bindings, _ := json.Marshal(old.Agents)
			_ = pg.UpdateTool(key, old.Description, old.Schema, bindings, old.Enabled)
		})
	}
	if err := pg.SeedTool(key, "fixture", json.RawMessage(`{"type":"object"}`), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	const description = "user prefix；实测能登录后用 verify=true 重新登记或让平台标记 verified。user suffix"
	customSchema := json.RawMessage(`{"type":"object","properties":{"secret":{"type":"string","description":"custom secret instructions"},"target":{"type":"string","description":"custom target instructions"}},"required":["secret"]}`)
	if err := pg.UpdateTool(key, description, customSchema, json.RawMessage(`["reporter"]`), false); err != nil {
		t.Fatal(err)
	}
	s.upgradeCredentialToolMetadata()
	s.upgradeCredentialToolMetadata()
	row, err := pg.GetTool(key)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Enabled || len(row.Agents) != 1 || row.Agents[0] != "reporter" || !strings.HasPrefix(row.Description, "user prefix；") || !strings.HasSuffix(row.Description, "user suffix") || strings.Contains(row.Description, "verify=true") {
		t.Fatal("metadata upgrade overwrote tool permission choices or custom description")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(row.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "secret" || len(schema.Properties["source_url"]) == 0 || !strings.Contains(string(schema.Properties["secret"]), "custom secret instructions") || !strings.Contains(string(schema.Properties["target"]), "custom target instructions") {
		t.Fatal("metadata upgrade replaced existing schema properties instead of adding optional metadata")
	}
}
