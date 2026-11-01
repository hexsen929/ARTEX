package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func memoryEmptyEndedStage(t *testing.T, d *DB, task *Task) int64 {
	t.Helper()
	intent, _ := memoryFixtureIntent(t, d, task, "running", 0)
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	return intent
}

// If discovery accidentally invokes the content builder, fail deterministically
// instead of relying on a machine-dependent benchmark or a huge SQL workload.
func memoryRejectBuild(t *testing.T, d *DB, intent int64) {
	t.Helper()
	var original string
	if err := d.QueryRow(`SELECT pg_get_functiondef('memory_build_stage_source(text,text)'::regprocedure)`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(original); err != nil {
			t.Error(err)
		}
	})
	query := fmt.Sprintf(`CREATE OR REPLACE FUNCTION memory_build_stage_source(kind_arg text,id_arg text)
	 RETURNS TABLE(source_kind text,source_id text,source_version bigint,source_fingerprint text,
	 origin_task_id bigint,origin_conversation_id bigint,agent_key text,session_id text,
	 source_state text,source_updated_at timestamptz,content_text text)
	 LANGUAGE plpgsql STABLE ROWS 1 SET search_path=public,pg_temp AS $$ BEGIN
	 IF kind_arg='intent_stage' AND id_arg='%d' THEN RAISE EXCEPTION 'discovery content build sentinel'; END IF;
	 RETURN; END $$`, intent)
	if _, err := d.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryDiscoveryPlanDoesNotExpandContent(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	memoryEmptyEndedStage(t, d, task)
	settings, err := d.GetMemorySettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var plan []byte
	err = d.QueryRowContext(t.Context(), `EXPLAIN (FORMAT JSON) `+memoryDiscoverySQL, false, settings.LearnAfter, task.ID, settings.Epoch, 4).Scan(&plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"memory_build_stage_source", "memory_stage_anchor_signature", "memory_stage_targets", "content_text"} {
		if strings.Contains(string(plan), forbidden) {
			t.Fatalf("candidate plan expanded %s", forbidden)
		}
	}
}

func TestMemoryDiscoveryWarmEmptyHasNoBuildOrWrite(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent := memoryEmptyEndedStage(t, d, task)
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 0 {
		t.Fatalf("empty first pass: %+v %v", found, err)
	}
	var token int64
	var checkedAt time.Time
	if err := d.QueryRow(`SELECT id,checked_at FROM memory_discovery_checks WHERE source_kind='intent_stage' AND source_id=$1 AND checked`, fmt.Sprint(intent)).Scan(&token, &checkedAt); err != nil {
		t.Fatal(err)
	}
	memoryRejectBuild(t, d, intent)
	for range 3 {
		if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 0 {
			t.Fatalf("warm pass rebuilt content: %+v %v", found, err)
		}
	}
	var afterToken int64
	var afterTime time.Time
	if err := d.QueryRow(`SELECT id,checked_at FROM memory_discovery_checks WHERE source_kind='intent_stage' AND source_id=$1`, fmt.Sprint(intent)).Scan(&afterToken, &afterTime); err != nil {
		t.Fatal(err)
	}
	if token != afterToken || !checkedAt.Equal(afterTime) {
		t.Fatal("unchanged empty stage caused a metadata write")
	}
}

func TestMemoryDiscoveryLateMemberInvalidatesEmptyCache(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent := memoryEmptyEndedStage(t, d, task)
	if _, err := d.FindMemorySources(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	var token int64
	if err := d.QueryRow(`SELECT id FROM memory_discovery_checks WHERE source_kind='intent_stage' AND source_id=$1`, fmt.Sprint(intent)).Scan(&token); err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(task.ExplorationID)
	fact, err := store.AddNode(KindFact, map[string]any{"summary": "bounded late tool compatibility observation", "confidence": "observed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(intent, RelYields, fact); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(`UPDATE memory_discovery_checks SET checked=true WHERE id=$1`, token)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	if n != 0 {
		t.Fatal("stale reader acknowledged a changed source")
	}
	found, err := d.FindMemorySources(t.Context(), 100)
	if err != nil || len(found) != 1 || found[0].ID != fmt.Sprint(intent) {
		t.Fatalf("late member remained hidden: %+v %v", found, err)
	}
}

func TestMemoryDiscoveryInvalidConditionEditInvalidatesCache(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 1)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{preconditions}','{"unsupported":true}'::jsonb) WHERE id=$1`, facts[0]); err != nil {
		t.Fatal(err)
	}
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 0 {
		t.Fatalf("invalid phase: %+v %v", found, err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=payload-'preconditions',content_version=content_version+1 WHERE id=$1`, facts[0]); err != nil {
		t.Fatal(err)
	}
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 1 {
		t.Fatalf("repaired condition was not discovered: %+v %v", found, err)
	}
}

func TestMemoryDiscoveryCompletedGenerationSkipsLargeContent(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent := memoryEmptyEndedStage(t, d, task)
	// Thousands of native facts may exist, but a completed generation must be
	// filtered by metadata before ANY fingerprint/body function runs.
	if _, err := d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state,origin)
	 SELECT $1,'fact',jsonb_build_object('summary','old bounded fixture observation','confidence','observed','detail',repeat('x',2048)),
	 'confirmed','worker' FROM generate_series(1,2000)`, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO memory_sources(source_kind,source_id,source_version,source_fingerprint,origin_task_id,
	 source_updated_at,last_completed_version) SELECT source_kind,source_id,source_version,'fixture',origin_task_id,
	 source_updated_at,source_version FROM memory_stage_metadata WHERE source_kind='intent_stage' AND source_id=$1`, fmt.Sprint(intent)); err != nil {
		t.Fatal(err)
	}
	memoryRejectBuild(t, d, intent)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if found, err := d.FindMemorySources(ctx, 100); err != nil || len(found) != 0 {
		t.Fatalf("completed generation rebuilt: %+v %v", found, err)
	}
}

func TestMemoryDiscoveryExplicitHistoryCanRecheckCachedEmpty(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent := memoryEmptyEndedStage(t, d, task)
	if _, err := d.FindMemorySources(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	memoryRejectBuild(t, d, intent)
	if _, err := d.FindMemorySources(t.Context(), 100); err != nil {
		t.Fatal("automatic scan ignored negative cache")
	}
	if _, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50); err == nil || !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("explicit history did not recheck: %v", err)
	}
}

func TestMemoryDiscoveryRespectsCancellation(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	memoryEmptyEndedStage(t, d, task)
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE memory_discovery_checks IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = d.FindMemorySources(ctx, 4)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("blocked source scan ignored deadline: %v", err)
	}
}

func TestMemoryDiscoveryBudgetPreflight(t *testing.T) {
	d := memoryFixtureDB(t)
	task := memoryFixtureTask(t, d)
	settings, _ := d.GetMemorySettings(t.Context())
	settings.MaxJobsPerHour = 1
	if _, err := d.UpdateMemorySettings(t.Context(), settings, settings.Epoch); err != nil {
		t.Fatal(err)
	}
	if allowed, err := d.MemoryDiscoveryAllowed(t.Context()); err != nil || !allowed {
		t.Fatalf("fresh budget: %v %v", allowed, err)
	}
	if _, err := d.Exec(`INSERT INTO memory_audit(task_id,action) VALUES($1,'extract_admitted')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := d.MemoryDiscoveryAllowed(t.Context()); err != nil || allowed {
		t.Fatalf("exhausted budget admitted discovery: %v %v", allowed, err)
	}
}

func TestMemoryDiscoveryEmptyCacheTracksAnchorAndAssetChanges(t *testing.T) {
	for _, mutation := range []string{"asset_edit", "asset_delete", "anchor_delete"} {
		t.Run(mutation, func(t *testing.T) {
			d := memoryFixtureDB(t)
			memoryEnableIncrementalHistory(t, d)
			task := memoryFixtureTask(t, d)
			intent, facts := memoryFixtureIntent(t, d, task, "running", 1)
			var asset int64
			if err := d.QueryRow(`INSERT INTO assets(type,url) VALUES('endpoint','https://fixture:fixture@target.invalid/') RETURNING id`).Scan(&asset); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := d.Exec(`DELETE FROM assets WHERE id=$1`, asset); err != nil {
					t.Error(err)
				}
			})
			if _, err := d.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2)`, facts[0], asset); err != nil {
				t.Fatal(err)
			}
			if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "done"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
				t.Fatal(err)
			}
			if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 0 {
				t.Fatalf("unsafe target was included: %+v %v", found, err)
			}
			var token int64
			if err := d.QueryRow(`SELECT id FROM memory_discovery_checks WHERE source_kind='intent_stage' AND source_id=$1 AND checked`, fmt.Sprint(intent)).Scan(&token); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mutation {
			case "asset_edit":
				_, err = d.Exec(`UPDATE assets SET url='https://target.invalid/' WHERE id=$1`, asset)
			case "asset_delete":
				_, err = d.Exec(`DELETE FROM assets WHERE id=$1`, asset)
			case "anchor_delete":
				_, err = d.Exec(`DELETE FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, facts[0], asset)
			}
			if err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := d.QueryRow(`SELECT count(*) FROM memory_discovery_checks WHERE id=$1`, token).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("%s did not invalidate empty cache: %d %v", mutation, remaining, err)
			}
			if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 1 {
				t.Fatalf("%s did not permit fresh discovery: %+v %v", mutation, found, err)
			}
		})
	}
}

func TestMemoryDiscoveryNegativeCacheHasLateCommitBackstop(t *testing.T) {
	d := memoryFixtureDB(t)
	memoryEnableIncrementalHistory(t, d)
	task := memoryFixtureTask(t, d)
	intent, facts := memoryFixtureIntent(t, d, task, "running", 1)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{preconditions}','{"unsupported":true}'::jsonb) WHERE id=$1`, facts[0]); err != nil {
		t.Fatal(err)
	}
	if err := d.Exploration(task.ExplorationID).SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET completed_at=now()-interval '31 seconds' WHERE id=$1`, intent); err != nil {
		t.Fatal(err)
	}
	// This writer's invalidation runs before the discovery marker exists. Its
	// eventual commit is not visible to the source snapshot built below.
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE exploration_nodes SET payload=payload-'preconditions',content_version=content_version+1 WHERE id=$1`, facts[0]); err != nil {
		t.Fatal(err)
	}
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 0 {
		t.Fatalf("uncommitted content escaped snapshot: %+v %v", found, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Explicit history is immediately fresh, while automatic empty-cache work
	// has a bounded quiet period rather than a permanent missed invalidation.
	if found, err := d.FindMemoryHistorySources(t.Context(), task.ID, 50); err != nil || len(found) != 1 {
		t.Fatalf("explicit history was stale: %+v %v", found, err)
	}
	if _, err := d.Exec(`UPDATE memory_discovery_checks SET checked_at=now()-interval '16 minutes' WHERE source_kind='intent_stage' AND source_id=$1`, fmt.Sprint(intent)); err != nil {
		t.Fatal(err)
	}
	if found, err := d.FindMemorySources(t.Context(), 100); err != nil || len(found) != 1 {
		t.Fatalf("late commit remained hidden after expiry: %+v %v", found, err)
	}
}
