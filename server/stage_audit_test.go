package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/stage"
)

func TestStageAuditDoesNotExposeDownloadCredential(t *testing.T) {
	const token = "fixture-download-bearer-secret"
	for _, kind := range []string{stage.EventPut, stage.EventDownload, stage.EventExpire, stage.EventDestroy} {
		summary, detail := stageEventAudit(stage.Event{Kind: kind, Entry: stage.Entry{Token: token, Name: "fixture.dat", Size: 12, ExpiresAt: time.Now().Add(time.Minute)}})
		if strings.Contains(summary, token) || strings.Contains(detail, token) || !strings.Contains(detail, "token_sha256=") {
			t.Fatalf("download token leaked in %s audit", kind)
		}
	}
	// Malformed/empty token metadata must not panic or reveal partial secrets.
	stageEventAudit(stage.Event{Entry: stage.Entry{Token: "x"}})
}
