package db

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestClearMissingSkillNoticesPreservesLedgerAndColdArchive(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	previous, existed, err := d.GetSetting(skillMissingNoticesClearedThrough)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteSetting(skillMissingNoticesClearedThrough); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = d.SetSetting(skillMissingNoticesClearedThrough, previous)
		} else {
			_ = d.DeleteSetting(skillMissingNoticesClearedThrough)
		}
	})
	task, err := d.CreateTaskWithOptions("skill notice cold archive", "local fixture", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("skill-notice-%d", task.ID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM skill_usage WHERE skill=$1`, name)
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
	})
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, found := range []bool{false, true} {
		if _, err := d.Exec(`INSERT INTO skill_usage(skill,agent_key,task_id,exploration_id,found,ts)
VALUES($1,'worker',$2,$3,$4,clock_timestamp()-interval '1 hour')`, name, task.ID, task.ExplorationID, found); err != nil {
			t.Fatal(err)
		}
	}
	wantCalls := func(want int) {
		t.Helper()
		stats, err := d.MissingSkillStats(200)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, stat := range stats {
			if stat.Skill == name {
				got = stat.Calls
			}
		}
		if got != want {
			t.Fatalf("unacknowledged %s calls=%d, want %d", name, got, want)
		}
	}
	wantCalls(1)
	if err := d.ClearMissingSkillUsage(); err != nil {
		t.Fatal(err)
	}
	wantCalls(0)
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM skill_usage WHERE skill=$1`, name).Scan(&count); err != nil || count != 2 {
		t.Fatalf("clear deleted raw ledger rows: count=%d err=%v", count, err)
	}
	cutoff, err := missingSkillNoticeCutoff(d)
	if err != nil || cutoff == nil {
		t.Fatalf("missing notice cutoff: %v %v", cutoff, err)
	}
	// Deterministic per-event timestamps: no sleeps or dependency on wall-clock
	// scheduling. The new miss must survive when mixed with an old miss in one
	// archive aggregate for the same skill.
	if _, err := d.Exec(`INSERT INTO skill_usage(skill,agent_key,task_id,exploration_id,found,ts)
VALUES($1,'planner',$2,$3,false,$4)`, name, task.ID, task.ExplorationID, cutoff.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	wantCalls(1)
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatalf("claim archive=%+v err=%v", claimed, err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rawRows []map[string]any
	if err := json.Unmarshal(snapshot.Tables["skill_usage"], &rawRows); err != nil || len(rawRows) != 3 {
		t.Fatalf("archive must retain all raw events: rows=%d err=%v", len(rawRows), err)
	}
	if _, ok := snapshot.AggregateStats["missing_skill_events"]; !ok {
		t.Fatal("new archive lacks per-event notice times")
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/fixture/skill-notices.tar.zst", "fixture", 100, 50); err != nil {
		t.Fatal(err)
	}
	wantCalls(1)
	before, err := d.GetTaskArchive(archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ClearMissingSkillUsage(); err != nil {
		t.Fatal(err)
	}
	wantCalls(0)
	after, err := d.GetTaskArchive(archive.ID)
	if err != nil || string(before.AggregateStats) != string(after.AggregateStats) || before.ArchivePath != after.ArchivePath || before.SHA256 != after.SHA256 {
		t.Fatalf("acknowledgement rewrote archived audit data: err=%v", err)
	}
	// Successful-call history is unaffected even after its task becomes cold.
	stats, err := d.SkillStats()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stat := range stats {
		if stat.Skill == name && stat.Calls == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("successful archived usage disappeared")
	}
}

func TestMissingNoticeMergeFiltersNewAndLegacyArchives(t *testing.T) {
	cutoff := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	old, newer := cutoff.Add(-time.Hour), cutoff.Add(time.Hour)
	archives := []taskArchiveAggregate{
		{MissingSkillStats: []SkillStat{{Skill: "mixed", Calls: 2, LastUsed: &newer}}, MissingSkillEvents: []archivedMissingSkillEvent{
			{Skill: "mixed", AgentKey: "worker", TS: old}, {Skill: "mixed", AgentKey: "planner", TS: newer},
		}},
		{MissingSkillStats: []SkillStat{{Skill: "legacy-old", Calls: 3, LastUsed: &old}}},
		{MissingSkillStats: []SkillStat{{Skill: "legacy-imported", Calls: 4, LastUsed: &newer}}},
		{MissingSkillStats: []SkillStat{{Skill: "legacy-no-time", Calls: 1}}},
	}
	filtered := mergeUnacknowledgedArchivedSkillStats(nil, archives, &cutoff)
	if len(filtered) != 2 || filtered[0].Skill != "legacy-imported" || filtered[0].Calls != 4 || filtered[1].Skill != "mixed" || filtered[1].Calls != 1 || len(filtered[1].Agents) != 1 || filtered[1].Agents[0] != "planner" {
		t.Fatalf("unexpected post-clear archive statistics: %+v", filtered)
	}
	// Filtering must not mutate the source aggregates used by other dashboards.
	if archives[0].MissingSkillStats[0].Calls != 2 || len(archives[1].MissingSkillStats) != 1 {
		t.Fatal("notice filter mutated archived audit aggregates")
	}
	all := mergeUnacknowledgedArchivedSkillStats(nil, archives, nil)
	if len(all) != 4 {
		t.Fatalf("first use lost legacy history: %+v", all)
	}
}

func TestInvalidMissingNoticeCutoffIsNotSilentlyIgnored(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	previous, existed, err := d.GetSetting(skillMissingNoticesClearedThrough)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = d.SetSetting(skillMissingNoticesClearedThrough, previous)
		} else {
			_ = d.DeleteSetting(skillMissingNoticesClearedThrough)
		}
	})
	if err := d.SetSetting(skillMissingNoticesClearedThrough, "invalid-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MissingSkillStats(20); err == nil {
		t.Fatal("invalid acknowledgement timestamp was silently ignored")
	}
}
