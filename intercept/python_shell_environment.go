package intercept

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

type reviewSourceInput struct{ ref scriptReference }

func pythonEnvironmentName(name string) bool {
	switch name {
	case "PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONUSERBASE", "HOME", "PYTHONSAFEPATH":
		return true
	}
	return false
}

// Inspect only preceding shell effects, called function bodies, and already
// captured source inputs. Merely defining a helper does not mutate Python's
// environment. No function or source is executed to establish this fact.
func pythonModuleShellEnvironmentUnknown(tree *syntax.File, call *syntax.CallExpr, name string, sources map[*syntax.CallExpr]reviewSourceInput, constants map[string]shellConstant, remaining *int) bool {
	if *remaining <= 0 {
		return true
	}
	own := map[*syntax.Assign]bool{}
	for _, a := range call.Assigns {
		own[a] = true
	}
	functions := map[string][]*syntax.FuncDecl{}
	active := map[*syntax.FuncDecl]bool{}
	sourceBytes := 0
	unknown := false
	var inspect func(syntax.Node, bool, bool, int)
	inspect = func(root syntax.Node, prefix, inside bool, depth int) {
		if depth > 4 {
			unknown = true
			return
		}
		syntax.Walk(root, func(node syntax.Node) bool {
			if node == nil {
				return true
			}
			if unknown {
				return false
			}
			*remaining--
			if *remaining < 0 {
				unknown = true
				return false
			}
			if prefix && node.Pos().After(call.Pos()) {
				return false
			}
			switch n := node.(type) {
			case *syntax.FuncDecl:
				functions[n.Name.Value] = append(functions[n.Name.Value], n)
				return false
			case *syntax.ForClause:
				if iter, ok := n.Loop.(*syntax.WordIter); ok && pythonEnvironmentName(iter.Name.Value) {
					unknown = true
				}
			case *syntax.Assign:
				if !own[n] && n.Name != nil && pythonEnvironmentName(n.Name.Value) {
					unknown = true
				}
			case *syntax.DeclClause:
				if n.Variant.Value == "nameref" {
					unknown = true
					break
				}
				for _, arg := range n.Args {
					if arg.Name != nil {
						if pythonEnvironmentName(arg.Name.Value) {
							unknown = true
						}
						continue
					}
					value, known := staticWord(arg.Value)
					variable, _, _ := strings.Cut(value, "=")
					if !known || pythonEnvironmentName(variable) || (strings.HasPrefix(value, "-") && strings.Contains(strings.TrimPrefix(value, "-"), "n")) {
						unknown = true
						break
					}
				}
			case *syntax.ParamExp:
				if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) && pythonEnvironmentName(n.Param.Value) {
					unknown = true
				}
			case *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.CStyleLoop:
				unknown = true
			case *syntax.CallExpr:
				if n == call {
					return false
				}
				args, known := constantCallArgs(n.Args, constants)
				if !known {
					unknown = true
					break
				}
				if len(args) == 0 {
					break
				}
				args, command, known := unwrapScriptCall(args)
				if !known {
					unknown = true
					break
				}
				if defs := functions[command]; len(defs) > 0 {
					if command == "cd" || command == "pushd" || command == "popd" {
						unknown = true
						break
					}
					for _, fn := range defs {
						if active[fn] {
							unknown = true
							break
						}
						active[fn] = true
						inspect(fn.Body, false, true, depth+1)
						delete(active, fn)
					}
					break
				}
				// The outer reference walker doesn't execute function bodies or
				// sourced programs to discover additional files. Do not label a
				// module capture complete while hiding another executed script.
				if inside && (strings.Contains(command, "/") || knownScriptExtension(command)) && !nativeInterpreterFile(command) {
					unknown = true
					break
				}
				if inside && scriptInterpreter.MatchString(filepath.Base(command)) {
					literalInline := false
					if len(args) > 1 {
						option, known := staticWord(args[1])
						if known && interpreterInfoOnly(filepath.Base(command), option) {
							literalInline = true
						}
						if known && (option == "-c" || option == "-e") && len(args) > 2 && strings.HasPrefix(filepath.Base(command), "python") {
							_, literalInline = staticWord(args[2])
						}
					}
					if !literalInline {
						unknown = true
						break
					}
				}
				switch command {
				case "eval", "trap", "alias", "let", "getopts", "enable":
					unknown = true
				case "pushd", "popd":
					unknown = true
				case "cd":
					if inside {
						unknown = true
					}
				case "source", ".":
					// Only sources whose path/content was captured at this exact call site
					// are accepted. Nested or dynamic sources retain the unknown status.
					input, known := sources[n]
					if !known || input.ref.problem != "" {
						unknown = true
						break
					}
					content := []byte(input.ref.content)
					if !input.ref.inline {
						var err error
						content, err = readScript(input.ref.path)
						if err != nil {
							unknown = true
							break
						}
					}
					sourceBytes += len(content)
					if sourceBytes > maxReviewScriptBytes || !utf8.Valid(content) || strings.ContainsRune(string(content), 0) {
						unknown = true
						break
					}
					file, err := syntax.NewParser().Parse(strings.NewReader(string(content)), "source")
					if err != nil {
						unknown = true
						break
					}
					inspect(file, false, true, depth+1)
				case "read", "readarray", "mapfile", "unset", "export", "readonly", "local", "declare", "typeset":
					for _, arg := range args[1:] {
						value, known := staticWord(arg)
						if !known {
							unknown = true
							break
						}
						variable, _, _ := strings.Cut(value, "=")
						if pythonEnvironmentName(variable) || (command == "unset" && variable == name) {
							unknown = true
							break
						}
						if strings.HasPrefix(value, "-") && (command == "declare" || command == "typeset" || command == "local") && strings.Contains(strings.TrimPrefix(value, "-"), "n") {
							unknown = true
							break
						}
					}
				case "printf":
					if len(args) > 1 {
						value, known := staticWord(args[1])
						if !known {
							unknown = true
							break
						}
						if strings.HasPrefix(value, "-v") {
							var variable string
							if value == "-v" {
								if len(args) < 3 {
									unknown = true
									break
								}
								variable, known = staticWord(args[2])
							} else {
								variable = strings.TrimPrefix(value, "-v")
							}
							if !known || pythonEnvironmentName(variable) {
								unknown = true
							}
						}
					}
				}
			}
			return !unknown
		})
	}
	inspect(tree, true, false, 0)
	// A function can shadow the interpreter without touching any environment
	// variable. Absolute/relative executable paths bypass shell functions.
	if !strings.Contains(name, string(filepath.Separator)) && len(functions[name]) > 0 {
		unknown = true
	}
	return unknown
}
