package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOperatorPayoutEligibilityAndOncePerTask(t *testing.T) {
	if moneyDirection("ordinary login bug") || !moneyDirection("充值接口") || !moneyDirection("USDT callback") {
		t.Fatal("payout eligibility differs")
	}
	s := &Scheduler{}
	if !s.markPayoutFired(1) || s.markPayoutFired(1) || s.markPayoutFired(0) || !s.markPayoutFired(2) {
		t.Fatal("payout once-per-task gate differs")
	}
}
func TestOperatorPageCacheMatchesOriginalPolicy(t *testing.T) {
	for name, want := range map[string]string{"_next/static/chunks/x.js": "public, max-age=31536000, immutable", "system/kb/index.html": "no-store", "index.html": "no-store", "favicon.ico": "no-store"} {
		w := httptest.NewRecorder()
		setWebUICacheHeaders(w, name)
		if got := w.Header().Get("Cache-Control"); got != want {
			t.Fatal(name, got)
		}
	}
}
func TestOperatorTriggerCapResetsWithRuntimeAndZeroIsUnlimited(t *testing.T) {
	s := operatorFixture(t)
	old, ok, err := s.m.pg.GetSetting("trigger_run_cap_exploit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			s.m.pg.SetSetting("trigger_run_cap_exploit", old)
		} else {
			s.m.pg.DeleteSetting("trigger_run_cap_exploit")
		}
	})
	if err = s.m.pg.SetSetting("trigger_run_cap_exploit", "1"); err != nil {
		t.Fatal(err)
	}
	task := time.Now().UnixNano()
	yes, err := s.admitOperatorTrigger(context.Background(), "exploit", task)
	if err != nil || !yes {
		t.Fatal(yes, err)
	}
	yes, err = s.admitOperatorTrigger(context.Background(), "exploit", task)
	if err != nil || yes {
		t.Fatal(yes, err)
	}
	fresh := &Server{m: s.m}
	yes, err = fresh.admitOperatorTrigger(context.Background(), "exploit", task)
	if err != nil || !yes {
		t.Fatal("runtime counter did not reset", yes, err)
	}
	s.m.pg.SetSetting("trigger_run_cap_exploit", "0")
	for i := 0; i < 3; i++ {
		yes, err = s.admitOperatorTrigger(context.Background(), "exploit", task)
		if err != nil || !yes {
			t.Fatal(yes, err)
		}
	}
}
