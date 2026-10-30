package server

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

func TestSkillDefaultsPendingCatalogStopsBeforeToolMetadata(t *testing.T) {
	pg := openIntranetTestDB(t)
	pg.SetMaxOpenConns(1)
	pg.SetMaxIdleConns(1)
	if _, err := pg.Exec(`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL); CREATE TEMP TABLE agent_skill_visibility (LIKE public.agent_skill_visibility INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DROP TABLE IF EXISTS pg_temp.agent_skill_visibility, pg_temp.settings`) })
	oldAugment := agent.ToolAugment
	t.Cleanup(func() { agent.ToolAugment = oldAugment })
	hostReads := 0
	wireAgentAugment(pg, t.TempDir()+"/not-installed", func() ([]actool.CoreTool, map[string][]string) {
		hostReads++
		return nil, nil
	})
	tools, info, cleanup := agent.ToolAugment(t.Context(), "worker")
	if cleanup != nil {
		defer cleanup()
	}
	if len(tools) != 0 || info.AssemblyError == nil || !strings.Contains(info.AssemblyError.Error(), "未完成初始化") || hostReads != 0 {
		t.Fatalf("pending defaults silently assembled tools: count=%d err=%v hostReads=%d", len(tools), info.AssemblyError, hostReads)
	}
	// A successfully inspected empty catalog is different from an unknown root.
	// It may initialize the empty defaults once without granting new capabilities.
	if err := seedInstalledBuiltinSkillVisibility(pg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_, info, cleanup = agent.ToolAugment(t.Context(), "worker")
	if cleanup != nil {
		defer cleanup()
	}
	if info.AssemblyError != nil {
		t.Fatalf("explicitly initialized empty defaults were blocked: %v", info.AssemblyError)
	}
}
