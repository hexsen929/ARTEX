package agent

import (
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
	"strings"
	"testing"
)

func TestFindingVerificationToolScopeAndEvidence(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tk, err := d.CreateTask("verification tool fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(t.Context(), db.RecordFindingInput{TaskID: tk.ID, Summary: "lead", Severity: "high"})
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(nil, "")
	ts.ts = es
	ts.taskID = tk.ID
	r, err := ts.verifyFinding().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"finding_id":%d,"status":"confirmed","note":"fixture comparison matches","version":0,"additional_evidence":"actual fixture result"}`, f.FindingID)), nil)
	if err != nil || r.IsError {
		t.Fatalf("verification %v %s", err, r.Flatten())
	}
	got, _ := d.GetFinding(f.FindingID)
	if got.VerificationStatus != "confirmed" || got.Severity != "high" || !strings.Contains(got.Evidence, "actual fixture result") {
		t.Fatalf("updated %+v", got)
	}
	other, err := d.CreateTask("other fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	ts.ts = d.Exploration(other.ExplorationID)
	r, err = ts.verifyFinding().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"finding_id":%d,"status":"rejected","note":"not owned","version":1}`, f.FindingID)), nil)
	if err != nil || !r.IsError {
		t.Fatalf("cross-task update accepted %v %s", err, r.Flatten())
	}
}
func TestCompactFindingPreservesVerification(t *testing.T) {
	n := &db.Node{Payload: json.RawMessage(`{"summary":"lead","verification_status":"blocked","verification_note":"needs baseline"}`), State: "confirmed"}
	if compactFinding(n)["verification_status"] != "blocked" {
		t.Fatal("legacy lifecycle overrode verification")
	}
	n.Payload = json.RawMessage(`{"summary":"old"}`)
	if compactFinding(n)["verification_status"] != "legacy" {
		t.Fatal("old node promoted")
	}
}

func TestReportFindingPreservesMissingProofAsUnverified(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tk, err := d.CreateTask("unverified preservation fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	ts := NewToolSet(d.Exploration(tk.ExplorationID), "fixture")
	ts.taskID = tk.ID
	for _, status := range []string{"confirmed", "blocked", "rejected"} {
		raw, _ := json.Marshal(map[string]any{"summary": "potential finding", "vulnclass": "fixture", "severity": "high", "verification_status": status})
		r, err := ts.addFinding().Call(t.Context(), raw, nil)
		if err != nil || r.IsError {
			t.Fatalf("lead discarded: %v %s", err, r.Flatten())
		}
		var response struct {
			db.RecordedFinding
			VerificationStatus string `json:"verification_status"`
		}
		if json.Unmarshal([]byte(strings.SplitN(r.Flatten(), "\n", 2)[1]), &response) != nil {
			t.Fatal("bad response")
		}
		f, _ := d.GetFinding(response.FindingID)
		want := db.VerificationUnverified
		if status == "blocked" {
			want = db.VerificationBlocked
		}
		if f.VerificationStatus != want || f.Severity != "high" {
			t.Fatalf("incorrect preservation: %+v", f)
		}
	}
}
