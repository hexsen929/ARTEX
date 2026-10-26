package main

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/tools"
)

func TestDoctorToolNoteReportsPlatformWarning(t *testing.T) {
	r := tools.CheckResult{Entry: tools.Entry{Name: "fixture"}, Status: tools.StatusUnpinned, Path: "/fixture/tool", PlatformWarning: "manifest is linux/amd64, host is darwin/arm64"}
	note := doctorToolNote(r)
	if !strings.Contains(note, "darwin/arm64") || !strings.Contains(note, "平台待核对") {
		t.Fatalf("platform warning hidden: %q", note)
	}
}

func TestDoctorToolSummaryDoesNotCountIncompatibleOrCorruptTools(t *testing.T) {
	status, note := doctorToolSummary([]tools.CheckResult{
		{Entry: tools.Entry{Name: "good"}, Status: tools.StatusOK},
		{Entry: tools.Entry{Name: "wrong-architecture"}, Status: tools.StatusUnpinned, HostIncompatible: true, PlatformWarning: "linux/amd64 on darwin/arm64"},
		{Entry: tools.Entry{Name: "foreign-pin"}, Status: tools.StatusUnpinned, PlatformWarning: "platform pin missing"},
		{Entry: tools.Entry{Name: "corrupt"}, Status: tools.StatusMismatch},
		{Entry: tools.Entry{Name: "missing"}, Status: tools.StatusMissing},
	})
	if status != docWarn || !strings.Contains(note, "1/5") || !strings.Contains(note, "本机不可运行") {
		t.Fatalf("incompatible files were counted as ready: status=%v note=%q", status, note)
	}
}

func TestDoctorToolNoteLabelsTargetResources(t *testing.T) {
	note := doctorToolNote(tools.CheckResult{
		Entry:  tools.Entry{Name: "client", Execution: "target", Platform: "linux/amd64"},
		Status: tools.StatusOK,
	})
	if !strings.Contains(note, "目标侧资源[linux/amd64]") || strings.Contains(note, "本机工具") {
		t.Fatalf("target resource was presented as a host tool: %q", note)
	}
}

func TestDoctorDirectoryIsNotReportedAsHashVerified(t *testing.T) {
	status, note := doctorToolSummary([]tools.CheckResult{{Entry: tools.Entry{Name: "python-tools"}, Status: tools.StatusUnpinned}})
	if status != docOK || !strings.Contains(note, "未钉 sha256") || !strings.Contains(note, "目录仅查存在性") {
		t.Fatalf("unpinned directory was presented as integrity-verified: %q", note)
	}
}

func TestDoctorLoopbackMatchesListenerDefaults(t *testing.T) {
	for _, addr := range []string{":8787", "0.0.0.0:8787", "[::]:8787"} {
		if doctorLoopback(addr) {
			t.Errorf("%s is not loopback-only", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		if !doctorLoopback(addr) {
			t.Errorf("%s is loopback-only", addr)
		}
	}
}
