//go:build ignore

package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestWorkerSettlementSettingValidationAndPersistence(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	old, existed, err := m.pg.GetSetting(settingWorkerSettlementSeconds)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if existed {
			_ = m.pg.SetSetting(settingWorkerSettlementSeconds, old)
		} else {
			_, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", settingWorkerSettlementSeconds)
		}
	}()
	s := &Server{m: m, jwtKey: []byte("worker-checkpoint-fixture-key")}
	_, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", settingWorkerSettlementSeconds)
	if s.workerSettlementTimeout() != agent.DefaultWorkerSettlementTimeout {
		t.Fatal("default setting")
	}
	for _, body := range []string{`{"worker_settlement_seconds":0}`, `{"worker_settlement_seconds":-1}`, `{"worker_settlement_seconds":1801}`, `{"worker_settlement_seconds":9223372036854775807}`, `{"worker_settlement_seconds":15.5}`} {
		w := httptest.NewRecorder()
		s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid setting accepted: %s status=%d", body, w.Code)
		}
		if s.workerSettlementTimeout() != agent.DefaultWorkerSettlementTimeout {
			t.Fatal("invalid request changed stored setting")
		}
	}
	for _, seconds := range []int{15, 75, 300, 1800} {
		raw, _ := json.Marshal(map[string]int{settingWorkerSettlementSeconds: seconds})
		w := httptest.NewRecorder()
		s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(raw))))
		if w.Code != 200 || s.workerSettlementTimeout() != time.Duration(seconds)*time.Second {
			t.Fatalf("setting not persisted status=%d body=%s", w.Code, w.Body.String())
		}
		var returned map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &returned); err != nil {
			t.Fatal(err)
		}
		if returned[settingWorkerSettlementSeconds] != float64(seconds) {
			t.Fatal("API did not echo effective setting")
		}
	}
	for _, raw := range []string{"bad", "0", "9223372036854775807"} {
		if err := m.pg.SetSetting(settingWorkerSettlementSeconds, raw); err != nil {
			t.Fatal(err)
		}
		if s.workerSettlementTimeout() != agent.DefaultWorkerSettlementTimeout {
			t.Fatal("invalid stored setting bypassed bounds")
		}
	}
	if (*Server)(nil).workerSettlementTimeout() != agent.DefaultWorkerSettlementTimeout {
		t.Fatal("nil fallback")
	}
}
