package intercept

import (
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const maxInlineVariants = 64
const maxInlineParses = 256

// Inline code is a shell string, not a filename. Accept escaped source text
// and simple quoted parameters proven by finite, unmodified enclosing loops.
// Expansion has no host environment, subprocess or filesystem callbacks.
func inlineShellWords(w *syntax.Word, bindings map[string][]string) ([]string, bool) {
	if w == nil {
		return nil, false
	}
	var names []string
	seen := map[string]bool{}
	var validParts func([]syntax.WordPart, bool) bool
	validParts = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			switch p := part.(type) {
			case *syntax.Lit:
				if !quoted && strings.ContainsAny(p.Value, "*?[]~") {
					return false
				}
			case *syntax.SglQuoted:
			case *syntax.DblQuoted:
				if !validParts(p.Parts, true) {
					return false
				}
			case *syntax.ParamExp:
				if !quoted || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Names != 0 || p.Exp != nil || len(bindings[p.Param.Value]) == 0 {
					return false
				}
				if !seen[p.Param.Value] {
					names = append(names, p.Param.Value)
					seen[p.Param.Value] = true
				}
			default:
				return false
			}
		}
		return true
	}
	if !validParts(w.Parts, false) {
		return nil, false
	}
	envs := [][]string{nil}
	for _, name := range names {
		if len(envs) > maxInlineVariants/len(bindings[name]) {
			return nil, false
		}
		var next [][]string
		for _, env := range envs {
			for _, value := range bindings[name] {
				pairs := append([]string(nil), env...)
				next = append(next, append(pairs, name+"="+value))
			}
		}
		envs = next
	}
	var words []string
	for _, env := range envs {
		fields, err := expand.Fields(&expand.Config{Env: expand.ListEnviron(env...), NoUnset: true}, w)
		if err != nil || len(fields) != 1 || len(fields[0]) > maxReviewScriptBytes {
			return nil, false
		}
		words = append(words, fields[0])
	}
	return words, true
}

// Avoid guessing loop values in the presence of assignments or commands which
// can mutate shell variables. The analysis intentionally prefers incomplete
// capture to interpreting eval, source, arithmetic or arbitrary builtins.
func loopBindingWrites(tree *syntax.File, constants map[string]shellConstant) (map[string]bool, bool) {
	writes := map[string]bool{}
	unknown := false
	loops := map[string]int{}
	var parents []syntax.Node
	syntax.Walk(tree, func(node syntax.Node) bool {
		if node == nil {
			parent := parents[len(parents)-1]
			parents = parents[:len(parents)-1]
			if loop, ok := parent.(*syntax.ForClause); ok {
				if iter, ok := loop.Loop.(*syntax.WordIter); ok {
					loops[iter.Name.Value]--
				}
			}
			return true
		}
		parents = append(parents, node)
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil {
				writes[n.Name.Value] = true
			}
		case *syntax.ForClause:
			if iter, ok := n.Loop.(*syntax.WordIter); ok {
				if loops[iter.Name.Value] > 0 {
					writes[iter.Name.Value] = true
				}
				loops[iter.Name.Value]++
			}
		case *syntax.FuncDecl:
			unknown = true
		case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.CStyleLoop:
			unknown = true
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				writes[n.Param.Value] = true
			}
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				break
			}
			resolved, known := constantCallArgs(n.Args, constants)
			if !known {
				unknown = true
				return true
			}
			args, name, ok := unwrapScriptCall(resolved)
			if !ok {
				unknown = true
				return true
			}
			switch name {
			case "eval", "source", ".", "read", "readarray", "mapfile", "unset", "declare", "typeset", "local", "export", "let", "getopts", "trap", "alias":
				unknown = true
			case "printf":
				if len(args) > 1 {
					values, known := inlineShellWords(args[1], nil)
					if !known || strings.HasPrefix(values[0], "-v") {
						unknown = true
					}
				}
			}
		}
		return true
	})
	return writes, unknown
}

func forLoopBindings(loop *syntax.ForClause, bindings map[string][]string, writes map[string]bool, unknown bool) map[string][]string {
	next := make(map[string][]string, len(bindings)+1)
	for name, values := range bindings {
		next[name] = values
	}
	iter, ok := loop.Loop.(*syntax.WordIter)
	if !ok {
		return next
	}
	name := iter.Name.Value
	delete(next, name)
	if unknown || writes[name] || loop.Select || !iter.InPos.IsValid() || len(iter.Items) == 0 || len(iter.Items) > maxInlineVariants {
		return next
	}
	var values []string
	for _, item := range iter.Items {
		value, known := staticWord(item)
		if !known {
			return next
		}
		values = append(values, value)
	}
	next[name] = values
	return next
}

// Unwrap only options whose argument layout is known. Treat unsupported layouts
// as opaque instead of mistaking an option's value for the executed command.
func unwrapScriptCall(args []*syntax.Word) ([]*syntax.Word, string, bool) {
	for len(args) > 0 {
		name, ok := staticCommandName(args[0])
		if !ok {
			return args, "", false
		}
		wrapper := filepath.Base(name)
		switch wrapper {
		case "env", "sudo", "exec", "timeout", "nohup", "command", "builtin", "time":
		default:
			return args, name, true
		}
		original := args
		if len(args) == 1 && wrapper != "timeout" {
			return args, name, true
		}
		args = args[1:]
		for len(args) > 0 {
			value, known := staticWord(args[0])
			if !known {
				return args, "", false
			}
			if value == "--" {
				args = args[1:]
				break
			}
			if wrapper == "env" && strings.Contains(value, "=") && !strings.HasPrefix(value, "-") {
				args = args[1:]
				continue
			}
			if !strings.HasPrefix(value, "-") || value == "-" {
				break
			}
			consume := 1
			switch wrapper {
			case "timeout":
				switch value {
				case "-s", "--signal", "-k", "--kill-after":
					consume = 2
				case "--foreground", "--preserve-status", "--verbose", "-v":
				default:
					if !strings.HasPrefix(value, "--signal=") && !strings.HasPrefix(value, "--kill-after=") {
						return args, "", false
					}
				}
			case "time":
				switch value {
				case "-f", "--format", "-o", "--output":
					consume = 2
				case "-a", "--append", "-p", "--portability", "-q", "--quiet", "-v", "--verbose":
				case "--help", "--version":
					return original, name, true
				default:
					if !strings.HasPrefix(value, "--format=") && !strings.HasPrefix(value, "--output=") && !strings.HasPrefix(value, "-f") && !strings.HasPrefix(value, "-o") {
						return args, "", false
					}
				}
			case "sudo":
				switch value {
				case "-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--close-from", "-T", "--command-timeout":
					consume = 2
				case "-n", "--non-interactive", "-E", "--preserve-env", "-H", "--set-home", "-S", "--stdin", "-k", "--reset-timestamp":
				default:
					if !strings.HasPrefix(value, "--user=") && !strings.HasPrefix(value, "--group=") && !strings.HasPrefix(value, "--preserve-env=") {
						return args, "", false
					}
				}
			case "env":
				switch value {
				case "-u", "--unset":
					consume = 2
				case "-i", "--ignore-environment", "-0", "--null", "-v", "--debug":
				default:
					if !strings.HasPrefix(value, "--unset=") {
						return args, "", false
					}
				}
			case "exec":
				if value == "-a" {
					consume = 2
				} else if value != "-c" && value != "-l" {
					return args, "", false
				}
			case "command":
				if value == "-v" || value == "-V" {
					return original, name, true
				}
				if value != "-p" {
					return args, "", false
				}
			default:
				return args, "", false
			}
			if len(args) < consume {
				return args, "", false
			}
			args = args[consume:]
		}
		if len(args) == 0 && wrapper != "timeout" {
			return original, name, true
		}
		if wrapper == "timeout" {
			if len(args) == 0 {
				return args, "", false
			}
			if _, known := staticWord(args[0]); !known {
				return args, "", false
			}
			args = args[1:]
		}
	}
	return args, "", false
}

// Capture literal files created by a top-level `cat > file <<'EOF'` before a
// later interpreter call. These files do not exist yet during approval, and an
// existing on-disk version would be stale. Other writes remain explicitly opaque.
func generatedScriptWrites(stmt *syntax.Stmt, args []*syntax.Word, dir string, topLevel bool, generated map[string]scriptReference, allowSed bool) []scriptReference {
	var writes []scriptReference
	var input *syntax.Redirect
	appendOutput := false
	inputs, outputs := 0, 0
	for _, r := range stmt.Redirs {
		stdin := r.N == nil || r.N.Value == "0"
		stdout := r.N == nil || r.N.Value == "1"
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.RdrIn, syntax.DplIn:
			if stdin {
				inputs++
				input = r
			}
		case syntax.RdrOut, syntax.ClbOut, syntax.AppOut, syntax.RdrAll:
			if !stdout && r.Op != syntax.RdrAll {
				continue
			}
			outputs++
			appendOutput = appendOutput || r.Op == syntax.AppOut
			path, known := staticWord(r.Word)
			if !known || path == "" {
				continue
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if filepath.IsAbs(path) || dir != "" {
				absolute, err := filepath.Abs(path)
				if err != nil {
					continue
				}
				path = absolute
			}
			writes = append(writes, scriptReference{path: path, generated: true, problem: "命令内脚本写入内容或执行路径不确定"})
		}
	}
	if allowSed && len(writes) == 1 && outputs == 1 && inputs == 0 && !appendOutput && topLevel && !stmt.Background {
		content, problem, known := generatedSedSnapshot(stmt, args, dir, writes[0].path, generated)
		if known {
			writes[0].problem = ""
			writes[0].inline = true
			writes[0].content = content
			return writes
		}
		if problem != "" {
			writes[0].problem = problem
		}
	}
	if len(writes) != 1 || outputs != 1 || inputs != 1 || input == nil || (input.Op != syntax.Hdoc && input.Op != syntax.DashHdoc) || !topLevel || stmt.Background || len(args) != 1 {
		return writes
	}
	name, known := staticCommandName(args[0])
	if !known || filepath.Base(name) != "cat" {
		return writes
	}
	// Only a cat's stderr duplication is transparent. Replacing stdout (>&2),
	// closing descriptors, or combining output from other producers is opaque.
	for _, r := range stmt.Redirs {
		if r.Op == syntax.RdrAll {
			return writes
		}
		if r.Op == syntax.DplOut {
			target, known := staticWord(r.Word)
			if r.N == nil || r.N.Value != "2" || !known || target != "1" {
				return writes
			}
		}
	}
	quoted := false
	syntax.Walk(input.Word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			quoted = true
		case *syntax.Lit:
			quoted = quoted || strings.Contains(n.Value, "\\")
		}
		return true
	})
	body := input.Hdoc
	if body == nil {
		body = &syntax.Word{}
	}
	var content string
	if quoted {
		var b strings.Builder
		for _, part := range body.Parts {
			lit, ok := part.(*syntax.Lit)
			if !ok {
				return writes
			}
			b.WriteString(lit.Value)
		}
		content = b.String()
	} else {
		// An unquoted heredoc performs shell expansion. Accept literal bodies
		// only, and use heredoc quote/escape semantics without any execution.
		for _, part := range body.Parts {
			if _, ok := part.(*syntax.Lit); !ok {
				return writes
			}
		}
		var err error
		content, err = expand.Document(&expand.Config{NoUnset: true}, body)
		if err != nil {
			return writes
		}
	}
	if input.Op == syntax.DashHdoc {
		lines := strings.Split(content, "\n")
		for i := range lines {
			lines[i] = strings.TrimLeft(lines[i], "\t")
		}
		content = strings.Join(lines, "\n")
	}
	if appendOutput {
		previous := generated[writes[0].path]
		if previous.path != "" {
			if !previous.inline || previous.problem != "" {
				return writes
			}
			content = previous.content + content
		} else {
			if !filepath.IsAbs(writes[0].path) {
				return writes
			}
			old, err := readScript(writes[0].path)
			if err != nil && !os.IsNotExist(err) {
				writes[0].problem = "脚本追加前内容无法读取：" + err.Error()
				return writes
			}
			content = string(old) + content
		}
	}
	writes[0].problem = ""
	writes[0].inline = true
	writes[0].content = content
	return writes
}

// These preparations have a statically known success path and do not transform
// a following quoted heredoc. Arbitrary guards, pipelines and transforms are
// intentionally excluded.
func staticScriptPreparation(stmt *syntax.Stmt) bool {
	if stmt.Background {
		return false
	}
	if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		return binary.Op == syntax.AndStmt && staticScriptPreparation(binary.X) && staticScriptPreparation(binary.Y)
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return false
	}
	if len(call.Args) == 0 {
		if len(call.Assigns) == 0 {
			return false
		}
		for _, assign := range call.Assigns {
			if assign.Name == nil || assign.Array != nil || assign.Index != nil {
				return false
			}
			if _, known := staticWord(assign.Value); !known {
				return false
			}
		}
		return true
	}
	name, known := staticCommandName(call.Args[0])
	if !known {
		return false
	}
	if name == "cd" && len(call.Args) == 2 {
		path, known := staticWord(call.Args[1])
		return known && path != "-"
	}
	if name != "mkdir" || len(call.Args) < 3 {
		return false
	}
	flag, known := staticWord(call.Args[1])
	if !known || (flag != "-p" && flag != "--parents") {
		return false
	}
	for _, arg := range call.Args[2:] {
		if _, known := staticWord(arg); !known {
			return false
		}
	}
	return true
}

// Shell quote removal for literal words, including escaped spaces and ANSI-C
// quotes. Unquoted expansion syntax stays unresolved; no environment or code
// is consulted by the expander.
func literalShellParts(parts []syntax.WordPart) (string, bool) {
	var known func([]syntax.WordPart, bool) bool
	known = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			switch p := part.(type) {
			case *syntax.Lit:
				if !quoted && unquotedExpansion(p.Value) {
					return false
				}
			case *syntax.SglQuoted:
			case *syntax.DblQuoted:
				if !known(p.Parts, true) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !known(parts, false) {
		return "", false
	}
	fields, err := expand.Fields(&expand.Config{NoUnset: true}, &syntax.Word{Parts: parts})
	if err != nil || len(fields) != 1 {
		return "", false
	}
	return fields[0], true
}

func unquotedExpansion(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' {
			i++
			continue
		}
		if strings.ContainsRune("*?[]~", rune(value[i])) {
			return true
		}
	}
	return false
}

func interpreterInfoOnly(name, option string) bool {
	if strings.HasPrefix(name, "python") {
		switch option {
		case "-V", "--version", "-h", "--help", "--help-all", "--help-env", "--help-xoptions":
			return true
		}
	}
	if name == "node" || name == "nodejs" {
		switch option {
		case "-v", "--version", "-h", "--help":
			return true
		}
	}
	return false
}
