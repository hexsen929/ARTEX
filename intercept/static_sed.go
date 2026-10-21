package intercept

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type staticSedSubstitution struct {
	pattern     *regexp.Regexp
	replacement string
	global      bool
}

// Support a deliberately small sed language: ASCII literal substitutions with
// an optional dot wildcard, escaped literal dots/delimiters, and the g flag.
// No subprocess, regex backreferences, shell evaluation, or sed e/w commands.
func parseStaticSed(program string) ([]staticSedSubstitution, bool) {
	if len(program) > 8192 || !asciiText(program) {
		return nil, false
	}
	var subs []staticSedSubstitution
	for program = strings.TrimSpace(program); program != ""; program = strings.TrimSpace(program) {
		if len(subs) >= 32 || len(program) < 3 || program[0] != 's' || !strings.ContainsRune("/#|@%:", rune(program[1])) {
			return nil, false
		}
		delim := program[1]
		program = program[2:]
		var pattern strings.Builder
		closed := false
		for len(program) > 0 {
			c := program[0]
			program = program[1:]
			if c == delim {
				closed = true
				break
			}
			if c == '\\' {
				if len(program) == 0 || (program[0] != '.' && program[0] != delim) {
					return nil, false
				}
				pattern.WriteString(regexp.QuoteMeta(program[:1]))
				program = program[1:]
			} else if c == '.' {
				pattern.WriteByte('.')
			} else {
				if strings.ContainsRune("*[]^$\\", rune(c)) || c < ' ' {
					return nil, false
				}
				pattern.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
		if !closed || pattern.Len() == 0 || pattern.Len() > 4096 {
			return nil, false
		}
		end := strings.IndexByte(program, delim)
		if end < 0 || end > 4096 {
			return nil, false
		}
		replacement := program[:end]
		program = program[end+1:]
		if strings.ContainsAny(replacement, "\\&\n\r") {
			return nil, false
		}
		global := strings.HasPrefix(program, "g")
		if global {
			program = program[1:]
		}
		program = strings.TrimSpace(program)
		if program != "" {
			if program[0] != ';' {
				return nil, false
			}
			program = program[1:]
		}
		re, err := regexp.Compile(pattern.String())
		if err != nil {
			return nil, false
		}
		subs = append(subs, staticSedSubstitution{re, replacement, global})
	}
	return subs, len(subs) > 0
}

func asciiText(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] == 0 || text[i] >= 128 {
			return false
		}
	}
	return true
}

func applyStaticSed(content string, subs []staticSedSubstitution) (string, bool) {
	// GNU and BSD sed differ on unterminated input. Don't guess that output.
	if !asciiText(content) || (content != "" && !strings.HasSuffix(content, "\n")) {
		return "", false
	}
	for _, sub := range subs {
		var out strings.Builder
		write := func(s string) bool {
			if out.Len()+len(s) > maxReviewScriptBytes {
				return false
			}
			out.WriteString(s)
			return true
		}
		for _, line := range strings.SplitAfter(content, "\n") {
			for {
				loc := sub.pattern.FindStringIndex(line)
				if loc == nil {
					if !write(line) {
						return "", false
					}
					break
				}
				if !write(line[:loc[0]]) || !write(sub.replacement) {
					return "", false
				}
				line = line[loc[1]:]
				if !sub.global {
					if !write(line) {
						return "", false
					}
					break
				}
			}
		}
		content = out.String()
	}
	return content, true
}

func generatedSedSnapshot(stmt *syntax.Stmt, args []*syntax.Word, dir, target string, generated map[string]scriptReference) (string, string, bool) {
	if len(args) != 3 {
		return "", "", false
	}
	name, known := staticCommandName(args[0])
	if !known || (name != "sed" && name != "/bin/sed" && name != "/usr/bin/sed") {
		return "", "", false
	}
	executable, err := exec.LookPath(name)
	if err != nil {
		return "", "", false
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || (executable != "/bin/sed" && executable != "/usr/bin/sed") || !nativeInterpreterFile(executable) {
		return "", "", false
	}
	program, known := staticWord(args[1])
	if !known {
		return "", "", false
	}
	subs, known := parseStaticSed(program)
	if !known {
		return "", "", false
	}
	source, known := staticWord(args[2])
	if !known || source == "" || strings.HasPrefix(source, "-") {
		return "", "", false
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(dir, source)
	}
	source = filepath.Clean(source)
	// Shell redirection truncates before sed opens its input. Never use the old
	// bytes when input/output refer to the same file or symlink/hardlink aliases.
	if source == target {
		return "", "", false
	}
	a, ae := os.Stat(source)
	b, be := os.Stat(target)
	if ae == nil && be == nil && os.SameFile(a, b) {
		return "", "", false
	}
	for _, r := range stmt.Redirs {
		if r.Op == syntax.DplOut || r.Op == syntax.RdrAll {
			return "", "", false
		}
	}
	var content string
	if ref, ok := generated[source]; ok {
		if !ref.inline || ref.problem != "" {
			return "", "", false
		}
		content = ref.content
	} else {
		if !filepath.IsAbs(source) {
			return "", "", false
		}
		raw, err := readScript(source)
		if err != nil {
			return "", "固定替换的原脚本无法读取：" + err.Error(), false
		}
		content = string(raw)
	}
	result, known := applyStaticSed(content, subs)
	return result, "", known
}

func staticSedCommandUnshadowed(tree *syntax.File) bool {
	safe := true
	syntax.Walk(tree, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil && n.Name.Value == "PATH" {
				safe = false
			}
		case *syntax.FuncDecl:
			if n.Name.Value == "sed" {
				safe = false
			}
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				break
			}
			for _, word := range n.Args {
				if value, known := staticWord(word); known && (strings.HasPrefix(value, "PATH=") || filepath.Base(value) == "sudo") {
					safe = false
				}
			}
			_, name, known := unwrapScriptCall(n.Args)
			if !known {
				safe = false
				break
			}
			switch name {
			case "eval", "alias", "source", ".":
				safe = false
			}
		}
		return true
	})
	return safe
}

// A pre-existing input cannot be reused after an opaque predecessor which may
// have rewritten it or changed the shell's directory/tool lookup. Known writes
// from cat/sed remain represented by the virtual-file map; everything else is
// deliberately excluded instead of treating stale disk bytes as future output.
func staticSedPrefixKnown(tree *syntax.File, target *syntax.Stmt, remaining *int) bool {
	if *remaining <= 0 {
		return false
	}
	safe := true
	functions := map[string]bool{}
	syntax.Walk(tree, func(node syntax.Node) bool {
		if node == nil {
			return true
		}
		if *remaining <= 0 {
			safe = false
			return false
		}
		*remaining--
		if !safe || node.Pos().After(target.Pos()) {
			return false
		}
		switch n := node.(type) {
		case *syntax.FuncDecl:
			functions[n.Name.Value] = true
			return false
		case *syntax.Assign:
			if n.Name != nil && (n.Name.Value == "PATH" || n.Name.Value == "PWD" || n.Name.Value == "OLDPWD") {
				safe = false
			}
		case *syntax.ForClause, *syntax.IfClause, *syntax.CaseClause, *syntax.ArithmExp, *syntax.ArithmCmd:
			safe = false
		case *syntax.Stmt:
			if n == target {
				return false
			}
			if n.Background {
				safe = false
				return false
			}
			call, ok := n.Cmd.(*syntax.CallExpr)
			if !ok {
				break
			}
			if len(call.Args) == 0 {
				break
			}
			name, known := staticCommandName(call.Args[0])
			if !known || functions[name] {
				safe = false
				break
			}
			base := filepath.Base(name)
			switch base {
			case "cd", "echo":
				if name != base {
					safe = false
				}
			case "printf":
				if name != base || len(call.Args) < 2 {
					safe = false
					break
				}
				format, known := staticWord(call.Args[1])
				if !known || strings.HasPrefix(format, "-v") {
					safe = false
				}
			case "cat", "mkdir", "chmod", "sed":
				executable, err := exec.LookPath(name)
				if err != nil {
					safe = false
					break
				}
				executable, err = filepath.EvalSymlinks(executable)
				if err != nil || (executable != "/bin/"+base && executable != "/usr/bin/"+base) || !nativeInterpreterFile(executable) {
					safe = false
					break
				}
				if base == "sed" {
					if len(call.Args) != 3 || len(n.Redirs) == 0 {
						safe = false
						break
					}
					program, known := staticWord(call.Args[1])
					if !known {
						safe = false
						break
					}
					if _, known := parseStaticSed(program); !known {
						safe = false
					}
				}
			default:
				safe = false
			}
			for _, r := range n.Redirs {
				switch r.Op {
				case syntax.RdrOut, syntax.ClbOut, syntax.AppOut:
					_, known := staticWord(r.Word)
					if !known || (r.N != nil && r.N.Value != "1") {
						safe = false
					}
				case syntax.RdrAll, syntax.DplOut:
					safe = false
				}
			}
		}
		return safe
	})
	return safe
}
