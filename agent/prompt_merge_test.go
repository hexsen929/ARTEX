package agent

import (
	"strings"
	"testing"
)

func TestMergeOptionalOpsecTailPreservesVariantAndCustomBodies(t *testing.T) {
	oldOverride, oldOpsec := PromptOverride, OpsecEnabled
	t.Cleanup(func() { PromptOverride, OpsecEnabled = oldOverride, oldOpsec })
	for _, enabled := range []bool{false, true} {
		OpsecEnabled = func() bool { return enabled }
		for _, variantBody := range []string{"VARIANT {{.Intranet}}", "{{.Unknown}}", ""} {
			PromptOverride = func(key string) (string, bool) {
				if key == "worker.intranet" {
					return variantBody, variantBody != ""
				}
				if key == "worker" {
					return "CUSTOM {{.Intranet}}", true
				}
				return "", false
			}
			got := renderSystemVariant("worker", "intranet", "DEFAULT", WorkerVars{Intranet: true})
			wantBody := "CUSTOM true"
			if variantBody == "VARIANT {{.Intranet}}" {
				wantBody = "VARIANT true"
			}
			if !strings.HasPrefix(got, wantBody) {
				t.Fatalf("custom/variant body changed: %q", got)
			}
			wantCount := 0
			if enabled {
				wantCount = 1
			}
			if count := strings.Count(got, opsecPromptBlock); count != wantCount {
				t.Fatalf("enabled=%v variant=%q: OPSEC tail count=%d", enabled, variantBody, count)
			}
		}
	}
	OpsecEnabled = nil
	if got := renderSystem("worker", "DEFAULT", WorkerVars{}); strings.Contains(got, opsecPromptBlock) {
		t.Fatal("missing host must leave optional OPSEC off")
	}
}

func TestMergeBuiltinSeedsPreserveIntranetAndVerification(t *testing.T) {
	prompts := BuiltinPromptSeeds()
	if prompts["worker.intranet"] != workerIntranetDefaultTmpl || prompts["asset_mapper"] != assetMapperDefaultTmpl {
		t.Fatal("upstream asset mapper replaced the fork's intranet variant")
	}
	tools := builtinToolsByAgent()
	verify := NewToolSet(nil, "").verifyFinding().Name()
	found := false
	for _, tool := range tools["auto"] {
		found = found || tool.Name() == verify
	}
	if !found {
		t.Fatal("Auto lost the fork's verification registration tool")
	}
	if len(tools["asset_mapper"]) != 2 {
		t.Fatal("asset mapper must retain its read-only default set")
	}
	for _, tool := range tools["asset_mapper"] {
		if tool.Name() != "list_assets" && tool.Name() != "list_companies" {
			t.Fatalf("asset mapper gained a non-read-only default: %s", tool.Name())
		}
	}
}
