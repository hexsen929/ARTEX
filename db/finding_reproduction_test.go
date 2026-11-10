package db

import (
	"errors"
	"strconv"
	"testing"
)

func TestReproductionAtomicRecordingAndVersionedEditing(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("repro fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	r := &Reproduction{EntryPoint: "fixture", Steps: []ReproductionStep{{Title: "observe", Request: "GET /fixture", Response: "synthetic", Proves: "fixture only"}}}
	in := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, VulnClass: "fixture", Severity: "low", Summary: "fixture", Evidence: "observation", Reproduction: r}
	out, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFinding(out.FindingID)
	if err != nil || f.Reproduction == nil || f.Reproduction.Steps[0].Request != "GET /fixture" || f.VerificationStatus != VerificationUnverified {
		t.Fatal(f, err)
	}
	version := f.EvidenceVersion
	r.Steps[0].Title = "updated"
	if err = d.SaveFindingReproduction(t.Context(), f.ID, version, r); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveFindingReproduction(t.Context(), f.ID, version, r); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatal("stale overwrite allowed", err)
	}
	r.Steps[0].TrafficIDs = []string{"foreign-or-imagined-id"}
	if _, err = d.Exploration(task.ExplorationID).RecordFinding(t.Context(), in); !errors.Is(err, ErrReproduction) {
		t.Fatal("unbound traffic accepted", err)
	}
	var count int
	d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&count)
	if count != 1 {
		t.Fatal("invalid reference left partial finding", count)
	}
	fs, err := d.ListFindingsForExport(FindingFilter{TaskID: strconv.FormatInt(task.ID, 10)}, nil)
	if err != nil || len(fs) != 1 || fs[0].Reproduction == nil {
		t.Fatal("export lost reproduction", err)
	}
}
