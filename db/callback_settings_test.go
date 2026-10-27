package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/config"
)

func TestCallbackReadOnlyProbeMatchesRuntimeAndDoesNotChangeSettings(t *testing.T) {
	dsn := testDSN(t)
	pg, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	old, found, err := pg.GetSetting(config.CallbackAddrSetting)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			_ = pg.SetSetting(config.CallbackAddrSetting, old)
		} else {
			_ = pg.DeleteSetting(config.CallbackAddrSetting)
		}
	})
	t.Setenv("ARTEX_CALLBACK_ADDR", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"callback_addr":"file.callback.test:443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", path)
	for _, value := range []string{"database.callback.test:8443", ""} {
		if err := pg.SetSetting(config.CallbackAddrSetting, value); err != nil {
			t.Fatal(err)
		}
		runtime, err := config.ResolveCallbackAddr(pg)
		if err != nil {
			t.Fatal(err)
		}
		probe, err := ProbeCallbackAddr(dsn)
		if err != nil || probe != runtime {
			t.Fatalf("probe=%+v runtime=%+v err=%v", probe, runtime, err)
		}
		stored, exists, err := pg.GetSetting(config.CallbackAddrSetting)
		if err != nil || !exists || stored != value {
			t.Fatalf("probe changed setting: %q, %v, %v", stored, exists, err)
		}
	}
	if err := pg.DeleteSetting(config.CallbackAddrSetting); err != nil {
		t.Fatal(err)
	}
	if probe, err := ProbeCallbackAddr(dsn); err != nil || probe.Address != "file.callback.test:443" || probe.Source != "config" {
		t.Fatalf("missing key fallback: %+v %v", probe, err)
	}
	if _, exists, err := pg.GetSetting(config.CallbackAddrSetting); err != nil || exists {
		t.Fatalf("read-only probe seeded a setting: %v %v", exists, err)
	}
}
