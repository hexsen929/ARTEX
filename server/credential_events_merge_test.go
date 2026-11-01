package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestCredentialWakeDrainsCoalescedBurst(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	finding, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal(err)
	}
	taskID := *finding.TaskID
	pg := s.m.pg
	oldRecord, oldAgent, oldBind := pg.GetBool(db.SettingTaskIntelligence, false), pg.GetBool(db.SettingCredentialAgent, false), pg.GetBool(db.SettingCredentialTraffic, false)
	t.Cleanup(func() { _ = pg.SetIntelligenceSettings(context.Background(), &oldRecord, &oldAgent, &oldBind) })
	on, off := true, false
	if err = pg.SetIntelligenceSettings(t.Context(), &on, &on, &off); err != nil {
		t.Fatal(err)
	}
	agent, err := pg.GetAgentByKey(db.CredentialAgentKey)
	if err != nil {
		t.Fatal(err)
	}
	if agent == nil {
		agent, err = pg.CreateAgent(db.CredentialAgentKey, "credential fixture", "fixture")
		if err != nil {
			t.Fatal(err)
		}
	}
	oldEnabled := agent.Enabled
	if _, err = pg.Exec(`UPDATE agents SET enabled=true WHERE key=$1`, db.CredentialAgentKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`UPDATE agents SET enabled=$2 WHERE key=$1`, db.CredentialAgentKey, oldEnabled) })
	for i := 0; i < credentialDrainBatch+3; i++ {
		_, err = pg.RecordIntelligence(t.Context(), taskID, 0, "fixture", db.IntelligenceInput{Kind: "credential", Title: "drain fixture", Content: fmt.Sprintf(`{"username":"fixture-%d","password":"fixture-secret"}`, i), Evidence: "local fixture", Confidence: "observed"})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Server{m: s.m, ctx: ctx, credentialWake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); worker.runCredentialProcessor() }()
	t.Cleanup(func() { cancel(); <-done })
	// Many notifications collapse to one slot. The worker must carry a full
	// batch forward itself instead of leaving three rows until slow ticker.
	for i := 0; i < 100; i++ {
		wakeCredentialProcessor(worker.credentialWake)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		var ready int
		if err = pg.QueryRow(`SELECT count(*) FROM task_intelligence WHERE task_id=$1 AND kind='credential' AND credential_analysis->>'state'='ready'`, taskID).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready == credentialDrainBatch+3 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("coalesced burst stranded rows: ready=%d", ready)
		case <-tick.C:
		}
	}
}
