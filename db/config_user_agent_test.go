package db

import "testing"

func TestProfileUserAgentRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-user-agent", Format: "openai", Model: "test-model", APIKey: "fixture-key",
	})
	if err != nil {
		t.Fatalf("insert profile with default User-Agent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
			t.Errorf("delete test profile: %v", err)
		}
	})

	check := func(wantUA string, want1M bool, wantKey string) *LLMProfile {
		t.Helper()
		p, err := d.ProfileByID(id)
		if err != nil || p == nil {
			t.Fatalf("load profile: %v", err)
		}
		if p.UserAgent != wantUA || p.Context1M != want1M {
			t.Fatalf("profile User-Agent=%q context_1m=%v, want %q / %v", p.UserAgent, p.Context1M, wantUA, want1M)
		}
		if p.APIKey != wantKey {
			t.Fatal("profile API key was not preserved or updated as requested")
		}
		var stored string
		if err := d.QueryRow(`SELECT user_agent FROM llm_profiles WHERE id=$1`, id).Scan(&stored); err != nil {
			t.Fatalf("User-Agent must be a non-null string: %v", err)
		}
		if stored != wantUA {
			t.Fatalf("stored User-Agent=%q, want %q", stored, wantUA)
		}
		profiles, err := d.ListProfiles()
		if err != nil {
			t.Fatalf("list profiles: %v", err)
		}
		for _, listed := range profiles {
			if listed.ID == id {
				if listed.UserAgent != wantUA || listed.Context1M != want1M {
					t.Fatal("list profile lost User-Agent or 1M option")
				}
				return p
			}
		}
		t.Fatal("saved profile missing from list")
		return nil
	}

	p := check("", false, "fixture-key")
	// Empty APIKey exercises the update path that preserves the stored secret.
	p.APIKey, p.UserAgent, p.Context1M = "", "stored-probe/1.0", true
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("update custom User-Agent without changing key: %v", err)
	}
	p = check("stored-probe/1.0", true, "fixture-key")
	p.APIKey, p.UserAgent, p.Context1M = "", "", false
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("clear User-Agent without changing key: %v", err)
	}
	p = check("", false, "fixture-key")
	// Non-empty APIKey exercises the second UPDATE statement, including clearing UA.
	p.APIKey, p.UserAgent, p.Context1M = "fixture-key-2", "custom-probe/2.0", true
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("update custom User-Agent and key: %v", err)
	}
	p = check("custom-probe/2.0", true, "fixture-key-2")
	p.APIKey, p.UserAgent, p.Context1M = "fixture-key-3", "", false
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("clear User-Agent and update key: %v", err)
	}
	check("", false, "fixture-key-3")
}
