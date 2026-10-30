package guard

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractHostsKeepsShellCommandAndRedirectBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"redirect then source", `curl https://allowed.test/ > req.sh; source common.sh`, []string{"allowed.test"}},
		{"bare target then scripts", "curl allowed.test && bash req.sh\n. common.sh", []string{"allowed.test"}},
		{"pipeline local output", `curl allowed.test | cat > common.sh`, []string{"allowed.test"}},
		{"local input and output", `curl -o req.sh common.sh < common.sh; cat req.sh`, []string{"common.sh"}},
		{"wget output is local", `wget -O req.sh common.sh`, []string{"common.sh"}},
		{"curl remote name flag has no value", `curl -O req.sh`, []string{"req.sh"}},
		{"sh URL is a real network target", `curl https://req.sh/path; source common.sh`, []string{"req.sh"}},
		{"source followed by real sh host", `source common.sh; curl req.sh`, []string{"req.sh"}},
		{"ordinary arguments are not tools", `echo curl common.sh; printf '%s' req.sh`, nil},
		{"multiple actual targets", `nmap -oN scan.log one.test two.sh`, []string{"one.test", "two.sh"}},
		{"explicit wrappers", `env MODE=fixture sudo -u nobody /usr/bin/curl common.sh`, []string{"common.sh"}},
		{"wrapper reset", `time curl common.sh; env MODE=fixture bash req.sh`, []string{"common.sh"}},
		{"command substitution", `printf '%s' "$(curl common.sh)"`, []string{"common.sh"}},
		{"network in redirect substitution", `curl allowed.test > "$(curl common.sh)"`, []string{"allowed.test", "common.sh"}},
		{"quoted URL concatenation", `curl 'https://'req.sh/path`, []string{"req.sh"}},
		{"process substitution", `cat <(curl common.sh) > req.sh`, []string{"common.sh"}},
		{"env split string", `env -S 'curl common.sh'`, []string{"common.sh"}},
		{"wrapped env split with extra target", `sudo env -S curl common.sh`, []string{"common.sh"}},
		{"env split equals", `env --split-string='curl common.sh'`, []string{"common.sh"}},
		{"SSH script arguments are local names on remote shell", `ssh jump.test bash common.sh`, []string{"jump.test"}},
		{"SSH remote network command", `ssh jump.test 'curl common.sh'`, []string{"jump.test", "common.sh"}},
		{"SSH jump and destination", `ssh -J gateway.test jump.test bash common.sh`, []string{"gateway.test", "jump.test"}},
		{"shell options before inline code", `bash -o nounset -c 'curl common.sh'`, []string{"common.sh"}},
		{"wrapper long value option", `xargs --max-procs 2 curl common.sh`, []string{"common.sh"}},
		{"timeout duration before network command", `timeout 5 curl outside.sh`, []string{"outside.sh"}},
		{"timeout options and duration", `timeout --signal TERM -k 1s 5m curl outside.sh`, []string{"outside.sh"}},
		{"timeout option equals and separator", `timeout --signal=TERM -- 5 curl outside.sh`, []string{"outside.sh"}},
		{"timeout unknown duration keeps argv position", `timeout "$DURATION" curl outside.sh`, []string{"outside.sh"}},
		{"timeout script filename is not network", `timeout 5 bash req.sh; source common.sh`, nil},
		{"busybox network applet", `busybox wget outside.sh > req.sh; source common.sh`, []string{"outside.sh"}},
		{"toybox network applet", `/usr/bin/toybox nc outside.sh 443`, []string{"outside.sh"}},
		{"busybox shell applet", `busybox sh -c 'curl common.sh > req.sh; source helper.sh'`, []string{"common.sh"}},
		{"busybox help is not applet execution", `busybox --help wget outside.sh`, nil},
		{"busybox echo arguments remain data", `busybox echo wget outside.sh`, nil},
		{"wrapped stdbuf short option", `stdbuf -oL curl outside.sh`, []string{"outside.sh"}},
		{"stdbuf option values", `stdbuf --output L --error=0 wget outside.sh`, []string{"outside.sh"}},
		{"nested timeout and multicall wrappers", `env MODE=fixture timeout 5 toybox wget outside.sh`, []string{"outside.sh"}},
		{"exec argv zero option", `exec -a fixture-label curl outside.sh`, []string{"outside.sh"}},
		{"literal eval shell source", `eval 'curl outside.sh > req.sh; source common.sh'`, []string{"outside.sh"}},
		{"eval literal argv join", `builtin eval -- curl outside.sh`, []string{"outside.sh"}},
		{"eval echo remains data", `eval 'echo curl outside.sh'`, nil},
		{"echo wrappers remain data", `echo timeout 5 curl outside.sh; printf '%s' 'eval curl common.sh'`, nil},
		{"unknown executable keeps literal IP", `"$EXECUTABLE" 192.0.2.17`, []string{"192.0.2.17"}},
		{"literal inline shell", `bash -lc 'curl allowed.test > req.sh; source common.sh'`, []string{"allowed.test"}},
		{"interpreter heredoc", "bash <<'SH'\ncurl common.sh > req.sh; source helper.sh\nSH\n", []string{"common.sh"}},
		{"empty interpreter heredoc", "bash <<'SH'\nSH\n", nil},
		{"script construction is data", "cat > req.sh <<'SH'\ncurl common.sh\nSH\n", nil},
		{"inline shell stdin is data", "bash -c 'cat' <<'DATA'\ncurl common.sh\nDATA\n", nil},
		{"standalone IP remains checked", `echo 192.0.2.7; printf '%s' 2001:db8::8`, []string{"192.0.2.7", "2001:db8::8"}},
		{"IP loop literal remains checked", `for h in 192.0.2.8; do ping "$h"; done`, []string{"192.0.2.8"}},
		{"unknown output retains argument positions", `curl -o "$OUTPUT" common.sh`, []string{"common.sh"}},
		{"quoted literal concatenation", `curl "co""mmon.sh"; bash req.sh`, []string{"common.sh"}},
		{"malformed shell fallback keeps boundaries", `curl allowed.test > req.sh; source common.sh; echo '`, []string{"allowed.test"}},
		{"malformed shell retains URL", `curl https://allowed.test/ > req.sh; source common.sh; echo '`, []string{"allowed.test"}},
		{"malformed remote shell retains literal network subcommand", `ssh jump.test 'curl outside.sh`, []string{"jump.test", "outside.sh"}},
		{"malformed wrapped network command", `timeout 5 busybox wget outside.sh > req.sh; source common.sh; echo '`, []string{"outside.sh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractHosts(tc.command); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hosts=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractHostsDoesNotReadHostEnvironment(t *testing.T) {
	t.Setenv("ROE_HOST_FIXTURE", "outside.sh")
	if got := ExtractHosts(`curl "$ROE_HOST_FIXTURE"; source common.sh`); len(got) != 0 {
		t.Fatalf("scope extractor evaluated process environment: %v", got)
	}
}

func TestRoECompositeScriptNamesDoNotRequireScopeExpansion(t *testing.T) {
	g := roeGuard("strict", []ScopeRule{{Kind: "domain", Value: "allowed.test"}})
	if blocked, message := bashBlocked(t, g, `curl https://allowed.test/ > req.sh; source common.sh`); blocked {
		t.Fatalf("local filenames became out-of-scope network targets: %s", message)
	}
	if blocked, _ := bashBlocked(t, g, `curl https://req.sh/`); !blocked {
		t.Fatal("real .sh network target stopped receiving strict scope checks")
	}
}

func TestRoEMessageExplainsCurrentModeWithoutSuggestingDowngrade(t *testing.T) {
	for _, mode := range []string{"warn", "strict"} {
		t.Run(mode, func(t *testing.T) {
			message := roeMessage(mode, []string{"outside.sh"}, []ScopeRule{{Kind: "domain", Value: "allowed.test"}})
			if !strings.Contains(message, "roe_enforcement="+mode) || !strings.Contains(message, "授权范围") || !strings.Contains(message, "add_task_scope") {
				t.Fatalf("mode/target follow-up missing: %s", message)
			}
			for _, misleading := range []string{"改为 warn", "/off", "off(关闭)", "如何关闭"} {
				if strings.Contains(message, misleading) {
					t.Fatalf("scope diagnostic recommended disabling protection: %s", message)
				}
			}
			if mode == "warn" && (!strings.Contains(message, "仅告警") || !strings.Contains(message, "其它拦截与审批规则")) {
				t.Fatalf("warn was presented as a blocked operation: %s", message)
			}
			if mode == "strict" && !strings.Contains(message, "需人工审批") {
				t.Fatalf("strict omitted approval boundary: %s", message)
			}
		})
	}
}
