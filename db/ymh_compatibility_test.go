package db

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func portTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestYMHRuleSeedsDoNotCollideWithExistingV3(t *testing.T) {
	d := portTestDB(t)
	for _, key := range []string{"intercept_default_rules_v3", "intercept_ymh_default_rules_v3", "intercept_ymh_default_rules_v4", "intercept_ymh_default_rules_v5", "intercept_ymh_default_rules_v6", "intercept_ymh_default_rules_v7"} {
		v, ok, err := d.GetSetting(key)
		if err != nil || !ok || v != "done" {
			t.Fatalf("migration %s=%q ok=%v err=%v", key, v, ok, err)
		}
	}
	var oldDelete int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_rules WHERE name='[内置] 删除类接口路径' AND action='deny'`).Scan(&oldDelete); err != nil || oldDelete == 0 {
		t.Fatalf("original delete protection missing: %v", err)
	}
	var before int
	_ = d.QueryRow(`SELECT count(*) FROM intercept_rules`).Scan(&before)
	// Re-running the added group after an interrupted seed must not duplicate
	// earlier inserted rows, even though the pre-existing v3 flag is already set.
	if err := d.DeleteSetting("intercept_ymh_default_rules_v3"); err != nil {
		t.Fatal(err)
	}
	if err := d.seedYMHInterceptRulesV3(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_rules`).Scan(&after); err != nil || after != before {
		t.Fatalf("seed duplicated rows before=%d after=%d err=%v", before, after, err)
	}
}

func TestConsumeSettingIsOneUseUnderConcurrentRefresh(t *testing.T) {
	d := portTestDB(t)
	key := "refresh-fixture-" + t.Name()
	if err := d.SetSetting(key, "fixture-hash"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteSetting(key) })
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, ok, err := d.ConsumeSetting(key)
			if err != nil {
				t.Errorf("consume: %v", err)
			}
			if ok {
				if v != "fixture-hash" {
					t.Error("wrong consumed value")
				}
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("one-use setting consumed %d times", successes.Load())
	}
}

func TestFindingInlineMergeIsOptInAndRetainsOriginalSubmissions(t *testing.T) {
	d := portTestDB(t)
	old, present, err := d.GetSetting(SettingFindingInlineMerge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = d.SetSetting(SettingFindingInlineMerge, old)
		} else {
			_ = d.DeleteSetting(SettingFindingInlineMerge)
		}
	})
	if err = d.SetBool(SettingFindingInlineMerge, false); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("inline compatibility fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	asset, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: taskIDString(task.ID) + ".inline.invalid", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, asset) })
	store := d.Exploration(task.ExplorationID)
	record := func(summary, status, note string) *RecordedFinding {
		t.Helper()
		out, err := store.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, VulnClass: "xss", Name: "fixture", Summary: summary, Severity: "high", Evidence: "fixture evidence", AssetIDs: []int64{asset}, VerificationStatus: status, VerificationNote: note})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := record("original report one", VerificationUnverified, "")
	second := record("original report two", VerificationUnverified, "")
	if first.FindingID == second.FindingID || second.Merged {
		t.Fatal("default path discarded an independent raw report")
	}
	if err = d.SetBool(SettingFindingInlineMerge, true); err != nil {
		t.Fatal(err)
	}
	third := record("additional raw report", VerificationUnverified, "")
	if !third.Merged || third.FindingID != second.FindingID {
		t.Fatal("explicit inline merge not applied")
	}
	events, err := d.ListFindingInlineMergeEvents(second.FindingID)
	if err != nil || len(events) != 2 {
		t.Fatalf("raw history: %+v %v", events, err)
	}
	if events[0].Kind != "original" || !strings.Contains(string(events[0].Payload), "original report two") || events[1].Kind != "incoming" || !strings.Contains(string(events[1].Payload), "additional raw report") {
		t.Fatal("raw submissions not retained")
	}
	blocked := record("blocked report", VerificationBlocked, "needs separate validation")
	if blocked.Merged || blocked.FindingID == second.FindingID {
		t.Fatal("different verification states were collapsed")
	}
}

func TestTaskSessionBindingIsExplicitNotInherited(t *testing.T) {
	d := portTestDB(t)
	makeTask := func() *Task {
		t.Helper()
		task, err := d.CreateTask("session capability fixture", "fixture", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
		return task
	}
	owner, child, other := makeTask(), makeTask(), makeTask()
	// A read-only task relation does not grant control of the parent's sessions.
	if _, err := d.Exec(`INSERT INTO task_relations(task_id,source_task_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, child.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(d, []byte("fixture-only-key"))
	id, err := store.Create(t.Context(), &SessionRecord{Kind: "http_php", URL: "http://fixture.invalid", Secret: json.RawMessage(`{"fixture":true}`), CreatedByTask: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })
	for task, want := range map[int64]bool{owner.ID: true, child.ID: false, other.ID: false} {
		ok, err := store.TaskCanUse(t.Context(), id, task)
		if err != nil || ok != want {
			t.Fatalf("task %d use=%v want=%v err=%v", task, ok, want, err)
		}
	}
	if err = store.BindTask(t.Context(), id, child.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.BindTask(t.Context(), id, child.ID); err != nil {
		t.Fatal("binding is not idempotent", err)
	}
	list, err := store.ListForTask(t.Context(), child.ID, SessionAlive)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("bound list=%+v err=%v", list, err)
	}
	if err = store.UpdateStatus(t.Context(), id, SessionDead); err != nil {
		t.Fatal(err)
	}
	list, err = store.ListForTask(t.Context(), child.ID, SessionAlive)
	if err != nil || len(list) != 0 {
		t.Fatal("status filter ignored")
	}
	if err = store.BindTask(t.Context(), id, 999999999); err == nil {
		t.Fatal("missing task accepted")
	}
}

func TestFindingCheckUsesIndependentVerificationAndProtectsManualChanges(t *testing.T) {
	d := portTestDB(t)
	for _, mode := range []string{"verified", "false_positive", "inconclusive", "manual_change", "evidence_change"} {
		t.Run(mode, func(t *testing.T) {
			task, err := d.CreateTask("check compatibility fixture", "fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
			f, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, Summary: "fixture", Severity: "high", Evidence: "original evidence"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.DeleteFinding(f.FindingID) })
			check, conv, created, err := d.CreateFindingCheck(t.Context(), f.FindingID, FindingCheckKindVerify)
			if err != nil || !created {
				t.Fatalf("check creation: %v", err)
			}
			t.Cleanup(func() { _ = d.DeleteConversation(conv.ID) })
			if started, err := d.StartFindingCheck(t.Context(), check.ID); err != nil || !started {
				t.Fatalf("start: %v", err)
			}
			verdict := mode
			if mode == "manual_change" || mode == "evidence_change" {
				verdict = "verified"
			}
			if err = d.RecordFindingCheckResult(t.Context(), conv.ID, verdict, "comparison summary", "recorded comparison result"); err != nil {
				t.Fatal(err)
			}
			if mode == "manual_change" {
				if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationRejected, "human reviewed", 0); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "evidence_change" {
				if err = d.SetFindingVerification(t.Context(), f.FindingID, VerificationUnverified, "new evidence pending review", 0, "new evidence"); err != nil {
					t.Fatal(err)
				}
			}
			if err = d.FinishFindingCheck(check.ID, "completed", ""); err != nil {
				t.Fatal(err)
			}
			got, err := d.GetFinding(f.FindingID)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"verified": VerificationConfirmed, "false_positive": VerificationRejected, "inconclusive": VerificationUnverified, "manual_change": VerificationRejected, "evidence_change": VerificationUnverified}[mode]
			if got.VerificationStatus != want || got.Status != FindingPending {
				t.Fatalf("verification=%s triage=%s", got.VerificationStatus, got.Status)
			}
			if mode == "verified" || mode == "false_positive" {
				if !strings.Contains(got.Evidence, "original evidence") || !strings.Contains(got.Evidence, "recorded comparison result") {
					t.Fatal("check evidence not appended")
				}
			}
			if mode == "manual_change" && got.VerificationNote != "human reviewed" {
				t.Fatal("late check overwrote manual conclusion")
			}
			if err = d.FinishFindingCheck(check.ID, "completed", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
