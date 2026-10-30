package agent

import (
	"strings"
	"testing"
)

func TestFindingRecordingDefaultsMatchPersistedVerificationStates(t *testing.T) {
	for name, body := range map[string]string{"worker": workerDefaultTmpl, "pentest": pentestDefaultTmpl} {
		t.Run(name, func(t *testing.T) {
			for _, state := range []string{"unverified", "blocked", "confirmed", "rejected", "verification_note"} {
				if !strings.Contains(body, state) {
					t.Errorf("recording guidance omits %s", state)
				}
			}
			for _, legacy := range []string{"别硬记成 finding", "别硬记成finding", "只有你本次真实触发过、拿到可复现证据（请求/响应或命令输出）才用"} {
				if strings.Contains(body, legacy) {
					t.Errorf("default still discards evidence-backed unverified findings: %s", legacy)
				}
			}
			if !strings.Contains(body, "无具体依据") || !strings.Contains(body, "不计为") {
				t.Error("defaults lost speculation and completion-evidence boundaries")
			}
		})
	}
}

func TestLegacyPromptRecordingClarificationDoesNotRequireDatabaseRewrite(t *testing.T) {
	for _, part := range []string{"旧版正文", "不得冒充 confirmed", "不是丢弃", "无依据猜测仍不是发现", "不要把待验证发现计为已确认漏洞或目标达成证据"} {
		if !strings.Contains(verificationGuidance, part) {
			t.Errorf("runtime clarification missing %q", part)
		}
	}
}
