package guard

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// hostTools are the common interaction tools whose positional argument is a
// target host / host:port / IP literal.
var hostTools = map[string]bool{
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true,
	"nmap": true, "ssh": true, "ping": true, "telnet": true, "dig": true,
	"host": true, "nslookup": true, "masscan": true, "httpx": true,
	"hydra": true, "smbclient": true, "rpcclient": true, "rdesktop": true,
	"xfreerdp": true,
}

// valueFlags take a non-host value as the next token; that token must not be
// mistaken for a target. 保守取向:宁缺勿滥。
var valueFlags = map[string]bool{
	"-p": true, "-o": true, "-oA": true, "-oX": true, "-oN": true, "-oG": true,
	"-iL": true, "-i": true, "-e": true, "-w": true, "-t": true, "-c": true,
	"-H": true, "-A": true, "-U": true, "-d": true, "-x": true, "-u": true,
	"-F": true, "-b": true, "-m": true, "-l": true, "-P": true,
	"--data": true, "--header": true, "--proxy": true, "--user-agent": true,
	"--connect-timeout": true, "--max-time": true, "--output": true,
	"--cacert": true, "--cert": true, "--key": true,
	"--top-ports": true, "--min-rate": true, "--max-rate": true,
}

var (
	reScheme = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s'"<>|\\^` + "`" + `]+`)
	// 主机名仅在网络命令参数位置识别；文件名不能靠后缀黑名单排除。
	reHostName = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*\.)+[a-z]{2,}$`)
)

// ExtractHosts extracts likely interaction targets from a shell command line:
// hosts of URLs (scheme://...), host:port / IP literals following common tool
// names, and standalone IP literals. 保守取向:提取不出 = 无目标(不拦截)。
// Returned hosts are lowercased, port-stripped, deduplicated, in first-seen order.
func ExtractHosts(command string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = normalizeHost(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}

	// Pass 1: URLs anywhere in the command.
	for _, raw := range reScheme.FindAllString(command, -1) {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			add(u.Hostname())
		}
	}

	// Parse each simple command independently. In particular, redirect paths and
	// a later `source common.sh` are not arguments of an earlier curl command.
	extractShellHosts(command, 0, add)
	return out
}

const maxHostShellDepth = 8

func extractShellHosts(command string, depth int, add func(string)) {
	if depth >= maxHostShellDepth {
		extractFallbackHosts(command, add)
		return
	}
	tree, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		// A partial/foreign shell is not a reason to discard obvious targets.
		// This fallback retains separators and excludes redirect operands.
		extractFallbackHosts(command, add)
		return
	}
	syntax.Walk(tree, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			words := make([]hostShellWord, len(n.Args))
			for i, word := range n.Args {
				words[i].value, words[i].known = hostLiteralWord(word)
			}
			extractCallHosts(words, depth, add)
		case *syntax.Stmt:
			call, ok := n.Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				break
			}
			words := make([]hostShellWord, len(call.Args))
			for i, word := range call.Args {
				words[i].value, words[i].known = hostLiteralWord(word)
			}
			start := hostCommandStart(words)
			if start >= len(words) || !words[start].known || !hostShellReadsStdin(words, start) {
				break
			}
			// Only an interpreter's literal stdin is executable shell; cat/tee
			// heredocs remain data. Command substitutions are visited by Walk.
			for _, redirect := range n.Redirs {
				if redirect.Hdoc == nil || len(redirect.Hdoc.Parts) == 0 {
					continue
				}
				start, end := int(redirect.Hdoc.Pos().Offset()), int(redirect.Hdoc.End().Offset())
				if start >= 0 && start <= end && end <= len(command) {
					extractShellHosts(command[start:end], depth+1, add)
				}
			}
		case *syntax.WordIter:
			// Keep the former conservative standalone-IP treatment for literal
			// loop values without evaluating variables or a target list file.
			for _, word := range n.Items {
				if value, ok := hostLiteralWord(word); ok {
					if ip := parseBareIP(value); ip != "" {
						add(ip)
					}
				}
			}
		}
		return true
	})
}

type hostShellWord struct {
	value string
	known bool
}

// Literal expansion has an explicitly empty environment and no execution or
// filesystem callbacks. Never resolve parameters, substitutions or globbing.
func hostLiteralWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var literal func([]syntax.WordPart) bool
	literal = func(parts []syntax.WordPart) bool {
		for _, part := range parts {
			switch p := part.(type) {
			case *syntax.Lit, *syntax.SglQuoted:
			case *syntax.DblQuoted:
				if !literal(p.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !literal(word.Parts) {
		return "", false
	}
	value, err := expand.Literal(&expand.Config{Env: expand.ListEnviron(), NoUnset: true}, word)
	return value, err == nil
}

func hostCommandStart(words []hostShellWord) int {
	i := 0
	for i < len(words) && words[i].known && isHostWrapper(baseOf(words[i].value)) {
		wrapper := baseOf(words[i].value)
		wrapperIndex := i
		i++
		if wrapper == "busybox" || wrapper == "toybox" {
			// In their execution form the next argv is an applet, not data.
			// Top-level --help/--list/--install forms are not applet execution.
			if i < len(words) && words[i].known && strings.HasPrefix(words[i].value, "-") {
				return wrapperIndex
			}
			continue
		}
		for i < len(words) && words[i].known {
			arg := words[i].value
			if wrapper == "env" && (arg == "-S" || arg == "--split-string" || strings.HasPrefix(arg, "--split-string=")) {
				return wrapperIndex
			}
			if arg == "--" {
				i++
				break
			}
			if wrapper == "env" && strings.Contains(arg, "=") && !strings.HasPrefix(arg, "-") {
				i++
				continue
			}
			if !strings.HasPrefix(arg, "-") {
				break
			}
			i++
			if hostWrapperValueFlag(wrapper, arg) && i < len(words) {
				i++
			}
		}
		if wrapper == "timeout" && i < len(words) {
			// timeout has one mandatory duration before the command. Keep that
			// argv position even when its value is a parameter we do not evaluate.
			i++
		}
	}
	return i
}

func isHostWrapper(name string) bool {
	switch name {
	case "sudo", "doas", "env", "time", "nice", "xargs", "command", "builtin", "exec", "nohup", "timeout", "busybox", "toybox", "stdbuf":
		return true
	}
	return false
}

func hostWrapperValueFlag(wrapper, flag string) bool {
	switch wrapper {
	case "sudo":
		return flag == "-u" || flag == "--user" || flag == "-g" || flag == "--group" || flag == "-h" || flag == "--host" || flag == "-p" || flag == "--prompt" || flag == "-C" || flag == "-R" || flag == "-D"
	case "doas":
		return flag == "-u" || flag == "-C"
	case "env":
		return flag == "-u" || flag == "--unset" || flag == "-C" || flag == "--chdir" || flag == "-a" || flag == "--argv0"
	case "nice":
		return flag == "-n" || flag == "--adjustment"
	case "time":
		return flag == "-f" || flag == "--format" || flag == "-o" || flag == "--output"
	case "xargs":
		return flag == "-I" || flag == "--replace" || flag == "-L" || flag == "--max-lines" || flag == "-n" || flag == "--max-args" || flag == "-P" || flag == "--max-procs" || flag == "-s" || flag == "--max-chars" || flag == "-E" || flag == "--eof" || flag == "-d" || flag == "--delimiter" || flag == "-a" || flag == "--arg-file"
	case "timeout":
		return flag == "-s" || flag == "--signal" || flag == "-k" || flag == "--kill-after"
	case "stdbuf":
		return flag == "-i" || flag == "--input" || flag == "-o" || flag == "--output" || flag == "-e" || flag == "--error"
	case "exec":
		return flag == "-a"
	}
	return false
}

func isHostShell(name string) bool {
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh":
		return true
	}
	return false
}

func hostShellValueFlag(flag string) bool {
	return flag == "-o" || flag == "+o" || flag == "-O" || flag == "+O" || flag == "--rcfile" || flag == "--init-file"
}

func hostShellReadsStdin(words []hostShellWord, start int) bool {
	if start >= len(words) || !words[start].known || !isHostShell(baseOf(words[start].value)) {
		return false
	}
	stdin := false
	for i := start + 1; i < len(words); i++ {
		word := words[i]
		if !word.known {
			return false
		}
		arg := word.value
		if hostShellValueFlag(arg) {
			i++
			continue
		}
		if arg == "--command" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c")) {
			return false
		}
		if arg == "-" {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "s") {
			stdin = true
		}
		if !strings.HasPrefix(arg, "-") {
			return stdin // a script filename unless -s selected stdin
		}
	}
	return true
}

func extractCallHosts(words []hostShellWord, depth int, add func(string)) {
	start := hostCommandStart(words)
	if start >= len(words) || !words[start].known {
		// Unknown executable names do not establish a bare-domain position, but
		// must not erase the existing conservative literal-IP observation.
		for _, word := range words {
			if word.known {
				if ip := parseBareIP(word.value); ip != "" {
					add(ip)
				}
			}
		}
		return
	}
	tool := baseOf(words[start].value)
	if tool == "eval" && depth < maxHostShellDepth {
		args := words[start+1:]
		if len(args) > 0 && args[0].known && args[0].value == "--" {
			args = args[1:]
		}
		allKnown := true
		parts := make([]string, len(args))
		for i, word := range args {
			if !word.known {
				allKnown = false
				break
			}
			parts[i] = word.value
		}
		if allKnown {
			// eval joins its literal argv with spaces and reparses as shell.
			// This is syntax inspection only, never execution or environment read.
			extractShellHosts(strings.Join(parts, " "), depth+1, add)
			return
		}
	}
	if tool == "env" && depth < maxHostShellDepth {
		for i := start + 1; i < len(words); i++ {
			if !words[i].known {
				continue
			}
			arg := words[i].value
			if arg == "-S" || arg == "--split-string" {
				if i+1 < len(words) && words[i+1].known {
					extractShellHosts(words[i+1].value+hostJoinedArgs(words[i+2:]), depth+1, add)
				}
				return
			}
			if strings.HasPrefix(arg, "--split-string=") {
				extractShellHosts(strings.TrimPrefix(arg, "--split-string=")+hostJoinedArgs(words[i+1:]), depth+1, add)
				return
			}
		}
	}
	if isHostShell(tool) && depth < maxHostShellDepth {
		for i := start + 1; i < len(words); i++ {
			if !words[i].known {
				break
			}
			flag := words[i].value
			if hostShellValueFlag(flag) {
				i++
				continue
			}
			if !strings.HasPrefix(flag, "-") && !strings.HasPrefix(flag, "+") {
				break // a filename, not inline shell source
			}
			if flag == "--command" || (!strings.HasPrefix(flag, "--") && strings.Contains(flag, "c")) {
				if i+1 < len(words) && words[i+1].known {
					extractShellHosts(words[i+1].value, depth+1, add)
				}
				return
			}
		}
	}
	network := hostTools[tool]
	skipNext := false
	sshDestination := false
	for i, word := range words[start+1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if !word.known {
			continue
		}
		tok := word.value
		if tool == "ssh" && sshDestination {
			// After the destination, SSH arguments are a remote shell command,
			// not additional target-host positions (e.g. `bash common.sh`).
			if depth < maxHostShellDepth {
				extractShellHosts(hostRemoteCommand(words[start+1+i:]), depth+1, add)
			}
			return
		}
		if tool == "ssh" && tok == "-J" {
			if start+2+i < len(words) && words[start+2+i].known {
				for _, jump := range strings.Split(words[start+2+i].value, ",") {
					if at := strings.LastIndex(jump, "@"); at >= 0 {
						jump = jump[at+1:]
					}
					if parseBareIP(jump) != "" || reHostName.MatchString(stripPortMaybe(jump)) {
						add(jump)
					}
				}
			}
			skipNext = true
			continue
		}
		if network && strings.HasPrefix(tok, "-") {
			skipNext = valueFlags[tok] || (tool == "wget" && tok == "-O")
			continue
		}
		if tool == "ssh" {
			sshDestination = true
		}
		if network {
			if at := strings.LastIndex(tok, "@"); at >= 0 {
				tok = tok[at+1:]
			}
		}
		if u, err := url.Parse(tok); network && err == nil && u.Hostname() != "" {
			add(u.Hostname())
		} else if ip := parseBareIP(tok); ip != "" {
			add(ip)
		} else if network && reHostName.MatchString(stripPortMaybe(tok)) {
			add(tok)
		}
	}
}

// SSH sends the remaining argv as one remote-shell command; unlike env -S's
// extra argv, their spaces must remain shell separators for static inspection.
func hostRemoteCommand(words []hostShellWord) string {
	parts := make([]string, len(words))
	for i, word := range words {
		parts[i] = "$__ROE_UNRESOLVED_ARGUMENT__"
		if word.known {
			parts[i] = word.value
		}
	}
	return strings.Join(parts, " ")
}

func hostJoinedArgs(words []hostShellWord) string {
	var out strings.Builder
	for _, word := range words {
		out.WriteByte(' ')
		if word.known {
			out.WriteString(strconv.Quote(word.value))
		} else {
			out.WriteString("$__ROE_UNRESOLVED_ARGUMENT__")
		}
	}
	return out.String()
}

var reFallbackSegments = regexp.MustCompile(`[|;&\r\n]+`)
var reFallbackWords = regexp.MustCompile(`[<>]+|[^\s<>]+`)

// Best effort only for malformed/unsupported shell. Unlike the old scanner,
// network-tool context cannot leak across a command separator or redirection.
func extractFallbackHosts(command string, add func(string)) {
	for _, segment := range reFallbackSegments.Split(command, -1) {
		args := []hostShellWord{}
		redirect := false
		for _, token := range reFallbackWords.FindAllString(segment, -1) {
			if strings.HasPrefix(token, "<") || strings.HasPrefix(token, ">") {
				redirect = true
				continue
			}
			if redirect {
				redirect = false
				continue
			}
			args = append(args, hostShellWord{value: strings.Trim(token, `"'`), known: true})
		}
		// A parser error may have left a leading quote/control token. Retain
		// obvious network calls rather than treating the whole source as empty.
		starts := []int{}
		for i, word := range args {
			if hostTools[baseOf(word.value)] {
				starts = append(starts, i)
			}
		}
		if len(starts) == 0 {
			extractCallHosts(args, maxHostShellDepth, add)
			continue
		}
		// Keep obvious literal network subcommands even after the recursion
		// ceiling or a malformed quoted remote-shell command. This fallback is
		// deliberately non-recursive and retains the separator/redirect limits.
		for i, start := range starts {
			end := len(args)
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			extractCallHosts(args[start:end], maxHostShellDepth, add)
		}
	}
}

func baseOf(tok string) string {
	if j := strings.LastIndex(tok, "/"); j >= 0 {
		return tok[j+1:]
	}
	return tok
}

// parseBareIP returns the normalized IP if tok is an IP literal or ip:port.
func parseBareIP(tok string) string {
	if host, _, err := net.SplitHostPort(tok); err == nil {
		tok = host
	}
	tok = strings.Trim(tok, "[]")
	if net.ParseIP(tok) != nil {
		return tok
	}
	return ""
}

// stripPortMaybe drops a trailing :<numeric-port> from host:port (best effort).
func stripPortMaybe(tok string) string {
	if host, port, err := net.SplitHostPort(tok); err == nil {
		if _, err := strconv.Atoi(port); err == nil {
			return host
		}
	}
	return tok
}
