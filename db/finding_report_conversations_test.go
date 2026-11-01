package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func reportBindingFixture(t *testing.T) (*DB, *Task, int64, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("report binding fixture", "fixture", nil, 0, 0)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	node, err := d.Exploration(task.ExplorationID).AddNode(KindFinding, map[string]any{"summary": "fixture"}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, node, "fixture", "fixture", "low", "fixture", "fixture evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM conversations WHERE id IN(SELECT conversation_id FROM conversation_activities WHERE kind=$1) AND title LIKE 'report-binding-fixture%'`, findingReportBindingKind)
		_, _ = d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
		_ = d.Close()
	})
	return d, task, fid, node
}

func reportFixtureConversation(t *testing.T, d *DB, role string) *Conversation {
	t.Helper()
	c, err := d.CreateConversation(role, "report-binding-fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(c.ID) })
	return c
}

func TestReportConversationExactHostBindingAndConcurrentResolve(t *testing.T) {
	d, task, fid, _ := reportBindingFixture(t)
	fake := reportFixtureConversation(t, d, "reporter")
	if err := d.RenameConversation(fake.ID, fmt.Sprintf("漏洞报告 · finding#%d0", fid)); err != nil {
		t.Fatal(err)
	}
	_, err := d.AppendConvActivity(fake.ID, Activity{Worker: "reporter", Kind: "user", Tool: "update_finding_report", Detail: fmt.Sprintf(`{"finding_id":%d,"task_id":%d}`, fid, task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE findings SET report='existing report' WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFinding(fid)
	if err != nil || f.ReportConversationID != nil {
		t.Fatal("title or ordinary content created an association", f, err)
	}
	if id, err := d.ResolveFindingReportConversation(fid); err != nil || id != nil {
		t.Fatal("historical substring inferred authority", id, err)
	}
	var wg sync.WaitGroup
	ids := make(chan int64, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, b, err := d.EnsureFindingReportConversation(t.Context(), fid)
			if err != nil {
				errs <- err
				return
			}
			if b.TaskID != task.ID || b.FindingID != fid {
				errs <- errors.New("wrong provenance")
				return
			}
			ids <- c.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var chosen int64
	for id := range ids {
		if chosen == 0 {
			chosen = id
		}
		if id != chosen || id == fake.ID {
			t.Fatal("resolve created multiple or adopted fabricated conversations")
		}
	}
	if chosen == 0 {
		t.Fatal("no resolved conversation")
	}
	t.Cleanup(func() { _ = d.DeleteConversation(chosen) })
	b, err := d.FindingReportBindingForConversation(t.Context(), chosen)
	if err != nil || b == nil || b.TaskID != task.ID || b.FindingID != fid {
		t.Fatal(b, err)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind=$2`, chosen, findingReportBindingKind).Scan(&count); err != nil || count != 1 {
		t.Fatal("resolve duplicated binding marker", count, err)
	}
	// A renamed title does not remove a structural association.
	if err = d.RenameConversation(chosen, "renamed by user"); err != nil {
		t.Fatal(err)
	}
	b, err = d.FindingReportBindingForConversation(t.Context(), chosen)
	if err != nil || b == nil {
		t.Fatal("renaming changed report authority", err)
	}
}

func TestReportConversationEvidenceAdmissionAndBodyAreAtomic(t *testing.T) {
	d, _, fid, node := reportBindingFixture(t)
	original, _, err := d.EnsureFindingReportConversation(t.Context(), fid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(original.ID) })
	other := reportFixtureConversation(t, d, "reporter")
	before, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	stale := before.EvidenceVersion + 1
	if _, err = d.SetFindingReportVersionAndConversationByNodeID(t.Context(), node, "stale report", &stale, other.ID); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatal("stale evidence admitted", err)
	}
	after, err := d.GetFinding(fid)
	if err != nil || after.Report != before.Report || after.ReportConversationID == nil || *after.ReportConversationID != original.ID {
		t.Fatal("failed report changed body or conversation", after, err)
	}
	b, err := d.FindingReportBindingForConversation(t.Context(), other.ID)
	if err != nil || b != nil {
		t.Fatal("rejected attempt gained an association", b, err)
	}
	if n, err := d.SetFindingReportVersionAndConversationByNodeID(t.Context(), node, "accepted report", &before.EvidenceVersion, other.ID); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	after, err = d.GetFinding(fid)
	if err != nil || after.Report != "accepted report" || after.ReportConversationID == nil || *after.ReportConversationID != other.ID {
		t.Fatal("accepted body/link not committed", after, err)
	}
	if _, err = d.FindingReportBindingForConversation(t.Context(), original.ID); err == nil {
		t.Fatal("replaced binding downgraded to ordinary chat")
	}
	// Existing non-reporter platform tools can still update reports, but are not
	// adopted as reporter sessions merely because they invoked the write tool.
	auto := reportFixtureConversation(t, d, "auto")
	if n, err := d.SetFindingReportVersionAndConversationByNodeID(t.Context(), node, "platform update", &before.EvidenceVersion, auto.ID); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	after, _ = d.GetFinding(fid)
	if *after.ReportConversationID != other.ID {
		t.Fatal("auto tool changed reporter ownership")
	}
}

func TestReportConversationSharedBatchGetsDedicatedFollowup(t *testing.T) {
	d, task, fid, node := reportBindingFixture(t)
	otherNode, err := d.Exploration(task.ExplorationID).AddNode(KindFinding, map[string]any{"summary": "second"}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := d.AddFinding(task.ID, otherNode, "fixture", "second", "low", "fixture", "evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := reportFixtureConversation(t, d, "reporter")
	for _, n := range []int64{node, otherNode} {
		if _, err = d.SetFindingReportVersionAndConversationByNodeID(t.Context(), n, "batch report", nil, shared.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.FindingReportBindingForConversation(t.Context(), shared.ID); err == nil {
		t.Fatal("multi-finding batch selected an arbitrary source")
	}
	for _, id := range []int64{fid, otherID} {
		c, b, err := d.EnsureFindingReportConversation(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteConversation(c.ID) })
		if c.ID == shared.ID || b.FindingID != id {
			t.Fatal("shared batch reused for a focused followup", c, b)
		}
	}
}

func TestReportConversationArchiveDropsUnprovenForeignKeysNotReport(t *testing.T) {
	d, task, fid, _ := reportBindingFixture(t)
	good, _, err := d.EnsureFindingReportConversation(t.Context(), fid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(good.ID) })
	fake := reportFixtureConversation(t, d, "reporter")
	deleted := reportFixtureConversation(t, d, "reporter")
	_ = d.DeleteConversation(deleted.ID)
	raw, err := json.Marshal([]map[string]any{
		{"id": fid, "task_id": task.ID, "report_conversation_id": good.ID, "report": "keep-good"},
		{"id": fid, "task_id": task.ID, "report_conversation_id": deleted.ID, "report": "keep-deleted"},
		{"id": fid, "task_id": task.ID, "report_conversation_id": fake.ID, "report": "keep-unproven"},
		{"id": fid + 1, "task_id": task.ID, "report_conversation_id": good.ID, "report": "keep-wrong-finding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	normalized, err := normalizeArchivedReportConversations(tx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ConversationID *int64 `json:"report_conversation_id"`
		Report         string `json:"report"`
	}
	if err = json.Unmarshal(normalized, &rows); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if row.Report == "" {
			t.Fatal("restore dropped report")
		}
		if i == 0 {
			if row.ConversationID == nil || *row.ConversationID != good.ID {
				t.Fatal("proved link lost")
			}
		} else if row.ConversationID != nil {
			t.Fatal("unproven link kept", i)
		}
	}
}
