package server

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestMergeScreenshotSettingDoesNotNarrowExplicitMCPGrant(t *testing.T) {
	pg := openIntranetTestDB(t)
	key := "merge_browser_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	a, err := pg.CreateAgent(key, "Browser merge fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE id=$1`, a.ID) })
	if err := pg.SetAgentSkillVisibility(a.ID, nil); err != nil {
		t.Fatal(err)
	}
	servers, err := pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	var original *db.MCPServer
	for _, server := range servers {
		if server.Name == browserMCPName {
			original = server
			break
		}
	}
	fixture := &db.MCPServer{Name: browserMCPName, Transport: "stdio", Command: "/fixture/must-not-launch", Enabled: true}
	var originalTools []db.MCPTool
	if original != nil {
		fixture.ID = original.ID
		originalTools, err = pg.MCPToolsDetailed(original.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	id, err := pg.SaveMCP(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture.ID = id
	t.Cleanup(func() {
		if original == nil {
			_ = pg.DeleteMCP(id)
			return
		}
		_, _ = pg.SaveMCP(original)
		_ = pg.SaveMCPTools(original.ID, originalTools)
	})
	const screenshot = "mcp__browser__browser_take_screenshot"
	if err := pg.SaveMCPTools(id, []db.MCPTool{{Name: screenshot, Description: "cached screenshot fixture", Schema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentVisibilityKind(a.ID, "mcp", []int64{id}); err != nil {
		t.Fatal(err)
	}
	oldSetting, existed, err := pg.GetSetting(settingBrowserScreenshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = pg.SetSetting(settingBrowserScreenshot, oldSetting)
		} else {
			_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, settingBrowserScreenshot)
		}
	})
	oldAugment := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = oldAugment })
	wireAgentAugment(pg, t.TempDir(), nil)
	for _, enabled := range []bool{false, true} {
		if err := pg.SetSetting(settingBrowserScreenshot, strconv.FormatBool(enabled)); err != nil {
			t.Fatal(err)
		}
		tools, info, cleanup := agent.ToolAugment(t.Context(), a.Key)
		if cleanup != nil {
			cleanup()
		}
		if info.AssemblyError != nil {
			t.Fatal(info.AssemblyError)
		}
		found := false
		for _, tool := range tools {
			found = found || tool.Name() == screenshot
		}
		if !found || !contains(info.GlobalNames, screenshot) {
			t.Fatalf("reporter setting %v removed an existing role's explicit browser capability", enabled)
		}
	}
}
