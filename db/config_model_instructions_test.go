package db

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestProfileModelInstructionsRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	previousActive, err := d.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}

	files := []string{"instruction_5.6.md", "custom.md"}
	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-model-instructions", Format: "openai-responses", Model: "test-model",
		APIKey: "fixture-key", UserAgent: "codex", Context1M: true,
		ModelInstructionsFile: files[0], ModelInstructionsFiles: files,
	})
	if err != nil {
		t.Fatalf("create profile with enabled model instruction file: %v", err)
	}
	t.Cleanup(func() {
		if previousActive != nil {
			if err := d.SetActiveProfile(previousActive.ID); err != nil {
				t.Errorf("restore active profile: %v", err)
			}
		}
		if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
			t.Errorf("delete model instruction fixture: %v", err)
		}
	})
	if err := d.SetActiveProfile(id); err != nil {
		t.Fatal(err)
	}

	check := func(wantFile string, wantFiles []string, wantKey string) *LLMProfile {
		t.Helper()
		checkFields := func(p *LLMProfile) {
			t.Helper()
			if p == nil || p.ModelInstructionsFile != wantFile || !reflect.DeepEqual(p.ModelInstructionsFiles, wantFiles) {
				t.Fatalf("model instruction fields did not round trip: got %#v, want %q / %v", p, wantFile, wantFiles)
			}
			if p.UserAgent != "codex" || !p.Context1M {
				t.Fatal("saving model instruction files changed User-Agent or 1M context")
			}
		}
		p, err := d.ProfileByID(id)
		if err != nil {
			t.Fatalf("load saved profile: %v", err)
		}
		checkFields(p)
		if p.APIKey != wantKey {
			t.Fatal("API key was not preserved or replaced as requested")
		}
		active, err := d.ActiveProfile()
		if err != nil || active == nil || active.ID != id {
			t.Fatalf("load active profile: %v", err)
		}
		checkFields(active)
		for _, load := range []func() ([]*LLMProfile, error){d.ListProfiles, d.PoolProfiles} {
			profiles, err := load()
			if err != nil {
				t.Fatalf("list profile instruction files: %v", err)
			}
			found := false
			for _, listed := range profiles {
				if listed.ID == id {
					checkFields(listed)
					found = true
				}
			}
			if !found {
				t.Fatal("saved profile missing from profile list")
			}
		}
		var storedFile, storedList, listType string
		if err := d.QueryRow(`SELECT model_instructions_file,model_instructions_files::text,jsonb_typeof(model_instructions_files) FROM llm_profiles WHERE id=$1`, id).Scan(&storedFile, &storedList, &listType); err != nil {
			t.Fatalf("read non-null stored instruction fields: %v", err)
		}
		if storedFile != wantFile || listType != "array" {
			t.Fatalf("stored instruction fields: file=%q list-type=%q", storedFile, listType)
		}
		var storedFiles []string
		if err := json.Unmarshal([]byte(storedList), &storedFiles); err != nil || !reflect.DeepEqual(storedFiles, wantFiles) {
			t.Fatalf("stored instruction array=%s: %v", storedList, err)
		}
		return p
	}

	p := check(files[0], files, "fixture-key")
	// Disable just the selected file while retaining every registered file and key.
	p.APIKey, p.ModelInstructionsFile = "", ""
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("disable model instructions without replacing key: %v", err)
	}
	p = check("", files, "fixture-key")
	// Select a different file through the UPDATE branch which replaces the key.
	p.APIKey, p.ModelInstructionsFile = "fixture-key-2", files[1]
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("enable alternate file and replace key: %v", err)
	}
	p = check(files[1], files, "fixture-key-2")
	// Nil and explicitly empty file lists both persist as JSON arrays.
	p.APIKey, p.ModelInstructionsFile, p.ModelInstructionsFiles = "", "", nil
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("clear registered files without replacing key: %v", err)
	}
	p = check("", []string{}, "fixture-key-2")
	p.APIKey, p.ModelInstructionsFiles = "fixture-key-3", []string{}
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatalf("save empty file list and replace key: %v", err)
	}
	check("", []string{}, "fixture-key-3")
}

func TestProfileModelInstructionsDefaults(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	for _, useSQLDefault := range []bool{false, true} {
		var id int64
		if useSQLDefault {
			err = d.QueryRow(`INSERT INTO llm_profiles(name,format,model) VALUES('t-model-instructions-sql-default','openai','test-model') RETURNING id`).Scan(&id)
		} else {
			id, err = d.SaveProfile(&LLMProfile{Name: "t-model-instructions-default", Format: "openai", Model: "test-model"})
		}
		if err != nil {
			t.Fatalf("insert profile with default instruction fields: %v", err)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
		p, err := d.ProfileByID(id)
		if err != nil || p == nil {
			t.Fatalf("load default profile: %v", err)
		}
		if p.ModelInstructionsFile != "" || p.ModelInstructionsFiles == nil || len(p.ModelInstructionsFiles) != 0 || p.CodexConfigFile != "" {
			t.Fatal("default instruction file must be empty and list must be a non-nil empty array")
		}
		serialized, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(serialized, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["model_instructions_files"]) != "[]" || string(fields["model_instructions_file"]) != `""` || string(fields["codex_config_file"]) != `""` {
			t.Fatalf("default instruction fields JSON: %s / %s", fields["model_instructions_file"], fields["model_instructions_files"])
		}
	}
}

func TestProfileCodexConfigFileRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	p := &LLMProfile{Name: "t-codex-config", Format: "openai-responses", Model: "fixture", APIKey: "fixture-key", UserAgent: "codex", CodexConfigFile: "/fixture/first.toml"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	check := func(path, key string) *LLMProfile {
		t.Helper()
		stored, err := d.ProfileByID(id)
		if err != nil || stored == nil || stored.CodexConfigFile != path || stored.APIKey != key {
			t.Fatalf("Codex config roundtrip path=%q error=%v", path, err)
		}
		for _, read := range []func() ([]*LLMProfile, error){d.ListProfiles, d.PoolProfiles} {
			profiles, err := read()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range profiles {
				if item.ID == id {
					found = true
					if item.CodexConfigFile != path {
						t.Fatalf("list path=%q want=%q", item.CodexConfigFile, path)
					}
				}
			}
			if !found {
				t.Fatal("saved Codex config missing from list")
			}
		}
		return stored
	}
	p = check("/fixture/first.toml", "fixture-key")
	p.APIKey, p.CodexConfigFile = "", "/fixture/second.toml"
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	p = check("/fixture/second.toml", "fixture-key")
	p.APIKey, p.CodexConfigFile = "fixture-key-2", ""
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	check("", "fixture-key-2")
}
