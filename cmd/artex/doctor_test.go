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
