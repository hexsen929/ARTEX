package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestProjectToolUsageDistinguishesDeclarationZeroAndUnavailable(t *testing.T) {
	rows := []*db.Tool{
		{Key: "external_fixture", Kind: "shell", Calls: 999},
		{Key: "callable_zero", Kind: "http", Calls: 999},
		{Key: "callable_used", Kind: "builtin", Calls: 999},
	}
	views := projectToolUsage(rows, map[string]int{"callable_used": 7}, nil)
	if len(views) != 3 || views[0].Calls != nil || views[0].UsageKind != "shell_declaration" || views[0].UsageAvailable {
		t.Fatal("shell declaration was presented as an unused callable tool")
	}
	if views[1].Calls == nil || *views[1].Calls != 0 || !views[1].UsageAvailable || views[1].UsageKind != "callable" {
		t.Fatal("a known zero must stay distinct from unknown statistics")
	}
	if views[2].Calls == nil || *views[2].Calls != 7 {
		t.Fatal("catalog count was replaced by a stale embedded value")
	}
	for i, view := range views {
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(raw, []byte(`"calls"`)) != 1 || bytes.Contains(raw, []byte(`999`)) {
			t.Fatalf("embedded legacy Calls leaked or duplicated the JSON field for row %d: %s", i, raw)
		}
		var projected map[string]json.RawMessage
		if err := json.Unmarshal(raw, &projected); err != nil {
			t.Fatal(err)
		}
		want := "null"
		if i == 1 {
			want = "0"
		} else if i == 2 {
			want = "7"
		}
		if string(projected["calls"]) != want {
			t.Fatalf("incorrect wire representation for row %d: %s", i, projected["calls"])
		}
	}
	failed := projectToolUsage(rows, map[string]int{"callable_used": 7}, errors.New("fixture stats unavailable"))
	for _, view := range failed {
		if view.Calls != nil || view.UsageAvailable {
			t.Fatal("failed statistics were silently represented as a real zero/count")
		}
		if view.Kind != "shell" && view.UsageError == "" {
			t.Fatal("callable statistics failure lost its availability explanation")
		}
	}
}

func TestBindingCapabilitiesExposeInheritanceAndExecutionCeilings(t *testing.T) {
	intranet := bindingCapabilities("worker.intranet")
	if intranet.ExecutionAgentKey != "worker" || intranet.ToolBindingMode != "inherited" || intranet.MCPBindingMode != "inherited" || intranet.SkillBindingMode != "direct" || intranet.RuntimeConfigMode != "inherited" || intranet.RuntimeConfigAgentKey != "worker" {
		t.Fatal("intranet binding metadata does not describe actual worker inheritance")
	}
	goals := bindingCapabilities("goals")
	if goals.ToolBindingMode != "limited" || goals.MCPBindingMode != "unsupported" || goals.SkillBindingMode != "readonly" || goals.RuntimeConfigMode != "llm_only" || len(goals.AllowedToolKeys) != 3 {
		t.Fatal("goals role lost its constrained assembly contract")
	}
	for _, key := range []string{"set_goals", "set_constraints", "add_task_scope"} {
		if !slices.Contains(goals.AllowedToolKeys, key) || !toolBindingSupported("goals", key) {
			t.Fatalf("goals base tool %s missing from the explicit ceiling", key)
		}
	}
	processor := bindingCapabilities(db.CredentialAgentKey)
	if processor.ToolBindingMode != "unsupported" || processor.MCPBindingMode != "unsupported" || processor.SkillBindingMode != "unsupported" || processor.RuntimeConfigMode != "llm_only" {
		t.Fatal("single-call credential processor was presented as an execution agent")
	}
	custom := bindingCapabilities("fixture_custom")
	if custom.ExecutionAgentKey != "fixture_custom" || custom.ToolBindingMode != "direct" || custom.MCPBindingMode != "direct" || custom.SkillBindingMode != "direct" || custom.RuntimeConfigMode != "direct" {
		t.Fatal("ordinary custom role inherited another role's capability ceiling")
	}
	for _, tool := range []string{"Bash", "shell_open", "external_fixture", "session_exec"} {
		if toolBindingSupported("goals", tool) || toolBindingSupported("worker.intranet", tool) || toolBindingSupported(db.CredentialAgentKey, tool) {
			t.Fatalf("unsupported execution grant %s accepted for a restricted/inherited role", tool)
		}
		if !toolBindingSupported("fixture_custom", tool) {
			t.Fatal("custom direct binding support was unexpectedly removed")
		}
	}
	legacy := []string{"goals", "worker.intranet", db.CredentialAgentKey}
	if err := validateNewToolBindings(nil, "external_fixture", legacy, legacy); err != nil {
		t.Fatal("saving an unchanged legacy selection tried to add or validate a new grant", err)
	}
	if err := validateNewToolBindings(nil, "external_fixture", legacy, nil); err != nil {
		t.Fatal("explicit removal of a legacy unsupported binding was rejected", err)
	}
}

func TestSkillListReportsLoadabilityAndUnavailableUsageSeparately(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom Skill root 中文")
	writeSkillInspectionFixture(t, root, "valid-index", "---\nname: declared-index\ndescription: fixture index\n---\nRead-only fixture.\n")
	writeSkillInspectionFixture(t, root, "broken-index", "---\nname: [broken\n---\nRead-only fixture.\n")
	if err := os.Mkdir(filepath.Join(root, "missing-index"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{skillDir: root}
	w := httptest.NewRecorder()
	s.fsListSkills(w, httptest.NewRequest("GET", "/api/skills", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Skill diagnostics should survive unavailable usage storage: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Skills            []skillFileNode `json:"skills"`
		UsageAvailable    bool            `json:"usage_available"`
		UsageError        string          `json:"usage_error"`
		RegistryAvailable bool            `json:"registry_available"`
		UsageScope        string          `json:"usage_scope"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 3 || result.UsageAvailable || result.UsageError == "" || !result.RegistryAvailable || result.UsageScope != "explicit_load_attempts_including_archives" {
		t.Fatal("Skill list lost registry/usage availability distinctions")
	}
	byName := map[string]skillFileNode{}
	for _, node := range result.Skills {
		byName[node.Name] = node
		if node.Calls != nil || node.Tasks != nil || node.UsageAvailable || node.UsageError == "" {
			t.Fatalf("unknown statistics were encoded as zero for %s", node.Name)
		}
	}
	valid := byName["valid-index"]
	if !valid.Loadable || valid.LoadError != "" || valid.DeclaredName != "declared-index" || !slices.Contains(valid.Files, "SKILL.md") {
		t.Fatal("valid Skill metadata/repair file view is incorrect")
	}
	broken := byName["broken-index"]
	if broken.Loadable || broken.LoadError == "" || !slices.Contains(broken.Files, "SKILL.md") {
		t.Fatal("broken metadata must remain visible for repair without being marked loadable")
	}
	missing := byName["missing-index"]
	if missing.Loadable || missing.LoadError == "" || len(missing.Files) != 0 {
		t.Fatal("missing index was presented as a healthy unused Skill")
	}
	var wire struct {
		Skills []map[string]json.RawMessage `json:"skills"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	for _, node := range wire.Skills {
		if string(node["calls"]) != "null" || string(node["tasks"]) != "null" {
			t.Fatal("unavailable Skill counters must be explicit JSON null")
		}
	}
}

func TestAgentAutomationClosedDatabaseIsUnknownNotDisabled(t *testing.T) {
	raw, err := sql.Open("pgx", "host=127.0.0.1 port=1 dbname=unused user=unused sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	// sql.Open is lazy. Closing before Query guarantees this fixture never dials.
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	pg := &db.DB{DB: raw}
	for _, key := range []string{db.FindingVerifierAgentKey, db.CredentialAgentKey} {
		dto := decorateAgentAutomation(pg, AgentDTO{Key: key, Enabled: true})
		if !dto.Enabled || dto.Automation == nil || dto.Automation.Available || dto.Automation.State != "unavailable" || dto.Automation.Reason == "" {
			t.Fatalf("failed switch query for %s was misreported as disabled: %+v", key, dto.Automation)
		}
		if dto.Automation.Kind == "" {
			t.Fatal("unknown state lost its pipeline identity")
		}
	}
	if dto := decorateAgentAutomation(pg, AgentDTO{Key: "worker", Enabled: true}); dto.Automation != nil {
		t.Fatal("ordinary worker received an unrelated automation status")
	}
}

func TestSkillRecentUsageScopeIsLiveAndAvailable(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires explicitly configured isolated test database")
	}
	pg := openIntranetTestDB(t)
	name := "module-usage-fixture-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM skill_usage WHERE skill=$1`, name) })
	for _, found := range []bool{true, false} {
		if err := pg.InsertSkillUsage(&db.SkillUsage{Skill: name, AgentKey: "worker", Found: found, SessionID: "fixture-session", ArgsLen: 9}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: &Manager{pg: pg}}
	r := httptest.NewRequest("GET", "/api/skills/"+name+"/usage?limit=10", nil)
	r.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.fsSkillUsage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("recent Skill usage failed: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Calls          []db.SkillCall `json:"calls"`
		Scope          string         `json:"scope"`
		HistoryScope   string         `json:"history_scope"`
		UsageAvailable bool           `json:"usage_available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scope != "live" || result.HistoryScope != "live" || !result.UsageAvailable || len(result.Calls) != 1 || result.Calls[0].AgentKey != "worker" {
		t.Fatal("recent matched calls were confused with full archived history or unresolved attempts")
	}
}

func TestAgentBindingPatchIsAtomicPartialAndPreservesMCPMetadata(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires explicitly configured isolated test database")
	}
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	key := "module_binding_fixture_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	a, err := pg.CreateAgent(key, "Binding fixture", "isolated temp-table regression")
	if err != nil {
		t.Fatal(err)
	}
	marker := "skill_visibility_initialized_v1:" + strconv.FormatInt(a.ID, 10)
	t.Cleanup(func() {
		_, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.agent_visibility, pg_temp.agent_skill_visibility`)
		_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, marker)
		_ = pg.DeleteAgent(key)
	})
	if _, err := pg.Exec(`CREATE TEMP TABLE agent_visibility (LIKE public.agent_visibility INCLUDING ALL);
CREATE TEMP TABLE agent_skill_visibility (LIKE public.agent_skill_visibility INCLUDING ALL);
ALTER TABLE agent_skill_visibility ADD CONSTRAINT module_fixture_skill_check CHECK(skill_name <> 'blocked')`); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pg.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name,enabled,created_at) VALUES($1,'mcp',77,'specific',true,$2)`, a.ID, created); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled,created_at) VALUES($1,'alpha',true,$2)`, a.ID, created); err != nil {
		t.Fatal(err)
	}
	type mcpRow struct {
		ResourceID int64
		ToolName   string
		Enabled    bool
		CreatedAt  time.Time
	}
	readMCP := func() []mcpRow {
		t.Helper()
		rows, err := pg.Query(`SELECT resource_id,mcp_tool_name,enabled,created_at FROM agent_visibility WHERE agent_id=$1 ORDER BY resource_id,mcp_tool_name`, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []mcpRow
		for rows.Next() {
			var item mcpRow
			if err := rows.Scan(&item.ResourceID, &item.ToolName, &item.Enabled, &item.CreatedAt); err != nil {
				t.Fatal(err)
			}
			result = append(result, item)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := readMCP()
	if len(before) != 1 || before[0].ToolName != "specific" || !before[0].CreatedAt.Equal(created) {
		t.Fatal("MCP metadata fixture is not initialized")
	}
	skills := []string{"alpha", "beta"}
	if err := pg.SetAgentBindings(t.Context(), a.ID, nil, &skills); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readMCP()) {
		t.Fatal("Skill-only patch rewrote MCP tool-specific grants or timestamps")
	}
	mcps := []int64{77}
	if err := pg.SetAgentBindings(t.Context(), a.ID, &mcps, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readMCP()) {
		t.Fatal("unchanged MCP selection rebuilt the existing specific grant")
	}
	if err := validateNewToolBindings(pg, "external_fixture", nil, []string{"goals"}); !errors.Is(err, errUnsupportedBinding) {
		t.Fatal("new execution declaration was accepted for goals", err)
	}
	variant := &db.Agent{ID: a.ID, Key: "worker.intranet"}
	if err := validateResourceBindingPatch(pg, variant, &mcps, nil); err != nil {
		t.Fatal("unchanged legacy inherited MCP grant was rejected", err)
	}
	addedMCP := []int64{77, 88}
	if err := validateResourceBindingPatch(pg, variant, &addedMCP, nil); !errors.Is(err, errUnsupportedBinding) {
		t.Fatal("new direct MCP grant accepted for inherited variant", err)
	}
	processor := &db.Agent{ID: a.ID, Key: db.CredentialAgentKey}
	if err := validateResourceBindingPatch(pg, processor, nil, &skills); err != nil {
		t.Fatal("unchanged legacy processor Skill selection was rejected", err)
	}
	addedSkill := append(append([]string{}, skills...), "additional")
	if err := validateResourceBindingPatch(pg, processor, nil, &addedSkill); !errors.Is(err, errUnsupportedBinding) {
		t.Fatal("new processor Skill grant was accepted", err)
	}
	blocked := []string{"blocked"}
	if err := pg.SetAgentBindings(t.Context(), a.ID, &addedMCP, &blocked); err == nil {
		t.Fatal("injected Skill constraint failure was not returned")
	}
	if !reflect.DeepEqual(before, readMCP()) {
		t.Fatal("failed combined patch committed its MCP half")
	}
	afterSkills, err := pg.AgentSkillNames(a.ID)
	if err != nil || !reflect.DeepEqual(afterSkills, skills) {
		t.Fatal("failed combined patch changed the previous Skill set", err)
	}
	emptyMCP, emptySkills := []int64{}, []string{}
	if err := pg.SetAgentBindings(t.Context(), a.ID, &emptyMCP, &emptySkills); err != nil {
		t.Fatal(err)
	}
	for _, row := range readMCP() {
		if row.Enabled {
			t.Fatal("explicit empty MCP patch did not disable all previous grants")
		}
	}
	afterSkills, err = pg.AgentSkillNames(a.ID)
	if err != nil || len(afterSkills) != 0 {
		t.Fatal("explicit empty Skill patch did not disable previous grants", err)
	}
	var initialized string
	if err := pg.QueryRow(`SELECT value FROM settings WHERE key=$1`, marker).Scan(&initialized); err != nil || initialized != "true" {
		t.Fatal("explicit empty Skill choice lost its initialization marker", err)
	}
}
