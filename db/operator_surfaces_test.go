package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestOperatorSurfaceOrderAndOverrides(t *testing.T) {
	for class, want := range map[string]string{"unauthorized API": "authz", "JWT token": "auth", "SQL injection": "injection", "file upload": "file", "SSRF": "request", "cache poisoning": "cache", "payment race": "logic", "GraphQL": "api", "XSS": "client", "config leak": "infra", "unrecognized fixture": "other"} {
		if got := SurfaceKeyFor(class); got != want {
			t.Errorf("%q: got %s want %s", class, got, want)
		}
	}
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	class := fmt.Sprintf("operator-surface-fixture-%d", time.Now().UnixNano())
	id, err := d.AddFinding(0, 0, class, "fixture", "low", "fixture", "fixture", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec("DELETE FROM findings WHERE id=$1", id)
	defer d.Exec("DELETE FROM finding_surface_overrides WHERE vulnclass=$1", class)
	if err = d.UpsertSurfaceOverrides(context.Background(), map[string]string{class: "logic"}); err != nil {
		t.Fatal(err)
	}
	fs, n, err := d.ListFindingsPage(FindingFilter{VulnClass: class, Surface: "logic"}, 1, 20)
	if err != nil || n != 1 || len(fs) != 1 || fs[0].Surface != "logic" {
		t.Fatal(n, fs, err)
	}
	_, n, err = d.ListFindingsPage(FindingFilter{VulnClass: class, Surface: "auth"}, 1, 20)
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if err = d.UpsertSurfaceOverrides(context.Background(), map[string]string{class: "bad-value"}); err == nil {
		t.Fatal("invalid surface accepted")
	}
}
