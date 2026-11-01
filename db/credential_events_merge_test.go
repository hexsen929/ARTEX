package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func credentialEventFixture(t *testing.T) (*DB, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	oldRecord, oldAgent, oldBind := d.GetBool(SettingTaskIntelligence, false), d.GetBool(SettingCredentialAgent, false), d.GetBool(SettingCredentialTraffic, false)
	t.Cleanup(func() { _ = d.SetIntelligenceSettings(context.Background(), &oldRecord, &oldAgent, &oldBind) })
	on, off := true, false
	if err = d.SetIntelligenceSettings(t.Context(), &on, &on, &off); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("credential event merge fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM conversations WHERE agent_key=$1 AND title LIKE $2`, CredentialAgentKey, fmt.Sprintf("凭据整理 · task#%d / intelligence#%%", task.ID))
		_, _ = d.Exec(`UPDATE tasks SET deleted_at=NULL,archived_at=NULL WHERE id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	return d, task.ID
}

func credentialEventItem(t *testing.T, d *DB, taskID int64, suffix string) *Intelligence {
	t.Helper()
	in := IntelligenceInput{Kind: "credential", Title: "credential-title-canary-" + suffix, Content: `{"username":"credential-user-canary","password":"credential-password-canary-` + suffix + `"}`, Evidence: "credential-evidence-canary", Confidence: "observed"}
	item, err := d.RecordIntelligence(t.Context(), taskID, 0, "fixture", in)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func credentialEventClaim(t *testing.T, d *DB, taskID int64, bind bool) *Intelligence {
	t.Helper()
	item, err := d.ClaimCredentialJob(t.Context(), bind, taskID)
	if err != nil || item == nil {
		t.Fatalf("claim: item=%v err=%v", item, err)
	}
	return item
}

func TestCredentialCompletionActivitiesAreMetadataOnlyAndSourceBound(t *testing.T) {
	d, taskID := credentialEventFixture(t)
	item := credentialEventItem(t, d, taskID, "first")
	job := credentialEventClaim(t, d, taskID, false)
	foreign, err := d.CreateConversation("auto", "unrelated fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(foreign.ID) })
	result := CredentialAnalysis{ConversationID: foreign.ID, State: "ready", Method: "agent", Model: "fixture-model", InputTokens: 17, OutputTokens: 9,
		Accounts: []CredentialAccount{{Username: "credential-user-canary", Password: "credential-password-canary-first", Token: "credential-token-canary", Cookie: "credential-cookie-canary"}},
		Error:    "credential-error-canary", Note: "credential-note-canary", TrafficNote: "credential-traffic-note-canary", TrafficState: "off"}
	accepted, err := d.FinishCredentialJobAccepted(t.Context(), job, result)
	if err != nil || !accepted {
		t.Fatalf("completion accepted=%v err=%v", accepted, err)
	}
	got, err := d.GetIntelligence(t.Context(), item.ID)
	if err != nil || got.Credentials == nil || got.Credentials.ConversationID <= 0 || got.Credentials.ConversationID == foreign.ID {
		t.Fatalf("wrong source pointer: %+v %v", got, err)
	}
	if got.Credentials.Accounts[0].Password != result.Accounts[0].Password {
		t.Fatal("source credential changed")
	}
	var all string
	if err = d.QueryRow(`SELECT c.title || COALESCE(string_agg(COALESCE(a.summary,'') || COALESCE(a.detail,''),''),'') FROM conversations c LEFT JOIN conversation_activities a ON a.conversation_id=c.id WHERE c.id=$1 GROUP BY c.id`, got.Credentials.ConversationID).Scan(&all); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-title-canary", "credential-user-canary", "credential-password-canary", "credential-token-canary", "credential-cookie-canary", "credential-error-canary", "credential-note-canary", "credential-traffic-note-canary", job.Credentials.Lease} {
		if strings.Contains(all, secret) {
			t.Fatalf("ordinary conversation copied secret canary %q", secret)
		}
	}
	if !strings.Contains(all, fmt.Sprintf(`"task_id":%d`, taskID)) || !strings.Contains(all, fmt.Sprintf(`"intelligence_id":%d`, item.ID)) {
		t.Fatal("source identity not recorded")
	}
	var foreignCount int
	if err = d.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, foreign.ID).Scan(&foreignCount); err != nil || foreignCount != 0 {
		t.Fatal("caller-selected conversation received output", err)
	}
	// Late traffic binding reuses only the true source conversation and must not
	// count the previous extraction's model tokens a second time.
	on := true
	if err = d.SetCredentialAgentSettings(t.Context(), nil, &on); err != nil {
		t.Fatal(err)
	}
	second := credentialEventClaim(t, d, taskID, true)
	result = *got.Credentials
	result.Error = ""
	result.TrafficState = "ready"
	if accepted, err = d.FinishCredentialJobAccepted(t.Context(), second, result); err != nil || !accepted {
		t.Fatal("late binding", accepted, err)
	}
	again, _ := d.GetIntelligence(t.Context(), item.ID)
	if again.Credentials.ConversationID != got.Credentials.ConversationID {
		t.Fatal("safe source conversation not reused")
	}
	var input, output, count int
	if err = d.QueryRow(`SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),count(*) FROM conversation_activities WHERE conversation_id=$1 AND tool='credential_processing_status'`, got.Credentials.ConversationID).Scan(&input, &output, &count); err != nil || input != 17 || output != 9 || count != 2 {
		t.Fatalf("duplicate usage: in=%d out=%d rows=%d err=%v", input, output, count, err)
	}
	if accepted, err = d.FinishCredentialJobAccepted(t.Context(), second, result); err != nil || accepted {
		t.Fatal("duplicate completion accepted", accepted, err)
	}
}

func TestCredentialCompletionRejectsLostAdmissionWithoutTranscript(t *testing.T) {
	for _, mutation := range []string{"disabled", "stale", "expired", "archived", "deleted", "source_changed"} {
		t.Run(mutation, func(t *testing.T) {
			d, taskID := credentialEventFixture(t)
			item := credentialEventItem(t, d, taskID, mutation)
			job := credentialEventClaim(t, d, taskID, false)
			var err error
			switch mutation {
			case "disabled":
				off := false
				err = d.SetCredentialAgentSettings(t.Context(), &off, nil)
			case "stale":
				_, err = d.Exec(`UPDATE task_intelligence SET credential_analysis=jsonb_set(credential_analysis,'{lease}','"replacement-fixture"'::jsonb) WHERE id=$1`, item.ID)
			case "expired":
				_, err = d.Exec(`UPDATE task_intelligence SET credential_analysis=jsonb_set(credential_analysis,'{lease_until}',to_jsonb(now()-interval '1 minute')) WHERE id=$1`, item.ID)
			case "archived":
				_, err = d.Exec(`UPDATE tasks SET archived_at=now() WHERE id=$1`, taskID)
			case "deleted":
				_, err = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, taskID)
			case "source_changed":
				_, err = d.Exec(`UPDATE task_intelligence SET content=content||' changed' WHERE id=$1`, item.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := d.FinishCredentialJobAccepted(t.Context(), job, CredentialAnalysis{State: "ready", Method: "rules", Accounts: []CredentialAccount{{Password: "rejected-canary"}}})
			if err != nil || accepted {
				t.Fatalf("lost admission published: accepted=%v err=%v", accepted, err)
			}
			var count int
			if err = d.QueryRow(`SELECT count(*) FROM conversations WHERE agent_key=$1 AND title LIKE $2`, CredentialAgentKey, fmt.Sprintf("凭据整理 · task#%d / intelligence#%%", taskID)).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected result created a conversation", count, err)
			}
		})
	}
}

func TestCredentialConversationRejectsForeignPersistedPointer(t *testing.T) {
	d, taskID := credentialEventFixture(t)
	first := credentialEventItem(t, d, taskID, "one")
	job := credentialEventClaim(t, d, taskID, false)
	result := CredentialAnalysis{State: "ready", Method: "rules", TrafficState: "off"}
	if ok, err := d.FinishCredentialJobAccepted(t.Context(), job, result); err != nil || !ok {
		t.Fatal(ok, err)
	}
	original, _ := d.GetIntelligence(t.Context(), first.ID)
	second := credentialEventItem(t, d, taskID, "two")
	raw, _ := json.Marshal(original.Credentials.ConversationID)
	if _, err := d.Exec(`UPDATE task_intelligence SET credential_analysis=jsonb_build_object('conversation_id',$2::jsonb,'state','queued') WHERE id=$1`, second.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	next := credentialEventClaim(t, d, taskID, false)
	if ok, err := d.FinishCredentialJobAccepted(t.Context(), next, result); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, _ := d.GetIntelligence(t.Context(), second.ID)
	if got.Credentials.ConversationID == original.Credentials.ConversationID {
		t.Fatal("foreign source pointer was reused")
	}
}
