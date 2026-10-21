package intercept

import (
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

type shellConstant struct {
	value string
	end   syntax.Pos
}

// Infer only single, literal assignments in the same straight-line shell scope.
// Values cannot be borrowed before assignment or across possible mutation.
func shellConstants(tree *syntax.File, straight map[*syntax.Stmt]bool) map[string]shellConstant {
	constants := map[string]shellConstant{}
	writes := map[string]int{}
	syntax.Walk(tree, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil {
				writes[n.Name.Value]++
			}
		case *syntax.ForClause:
			if iter, ok := n.Loop.(*syntax.WordIter); ok {
				writes[iter.Name.Value]++
			}
		case *syntax.ParamExp:
			if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				writes[n.Param.Value]++
			}
		case *syntax.Stmt:
			call, ok := n.Cmd.(*syntax.CallExpr)
			if !ok || !straight[n] || len(call.Args) != 0 {
				break
			}
			for _, assign := range call.Assigns {
				if assign.Name == nil || assign.Append || assign.Index != nil || assign.Array != nil {
					continue
				}
				value, known := staticWord(assign.Value)
				if assign.Value == nil {
					value, known = "", true
				}
				if known {
					constants[assign.Name.Value] = shellConstant{value: value, end: assign.End()}
				}
			}
		}
		return true
	})
	for name := range constants {
		if writes[name] != 1 {
			delete(constants, name)
		}
	}
	unknown := false
	syntax.Walk(tree, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl, *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.CStyleLoop:
			unknown = true
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				break
			}
			args, known := constantCallArgs(n.Args, constants)
			if !known {
				unknown = true
				break
			}
			args, name, known := unwrapScriptCall(args)
			if !known {
				unknown = true
				break
			}
			switch name {
			case "eval", "source", ".", "read", "readarray", "mapfile", "unset", "declare", "typeset", "local", "export", "let", "getopts", "trap", "alias":
				unknown = true
			case "printf":
				if len(args) > 1 {
					value, known := staticWord(args[1])
					if !known || strings.HasPrefix(value, "-v") {
						unknown = true
					}
				}
			}
		}
		return true
	})
	if unknown {
		return map[string]shellConstant{}
	}
	return constants
}

func constantWordFields(word *syntax.Word, constants map[string]shellConstant) ([]string, bool) {
	if word == nil {
		return nil, false
	}
	pairs := []string{}
	seen := map[string]bool{}
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
			case *syntax.ParamExp:
				value, ok := constants[p.Param.Value]
				if !ok || value.end.After(word.Pos()) || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Names != 0 || p.Exp != nil {
					return false
				}
				// Field splitting is modeled, pathname expansion is deliberately not.
				if !quoted && strings.ContainsAny(value.value, "*?[]") {
					return false
				}
				if !seen[p.Param.Value] {
					pairs = append(pairs, p.Param.Value+"="+value.value)
					seen[p.Param.Value] = true
				}
			default:
				return false
			}
		}
		return true
	}
	if !known(word.Parts, false) {
		return nil, false
	}
	if value, ok := constants["IFS"]; ok && !value.end.After(word.Pos()) && !seen["IFS"] {
		pairs = append(pairs, "IFS="+value.value)
	}
	fields, err := expand.Fields(&expand.Config{Env: expand.ListEnviron(pairs...), NoUnset: true}, word)
	return fields, err == nil
}

func exactShellWord(value string, pos syntax.Pos) *syntax.Word {
	// Expanded fields must not be reparsed as shell syntax or unescaped again.
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.SglQuoted{Left: pos, Value: value}}}
}

func constantCallArgs(args []*syntax.Word, constants map[string]shellConstant) ([]*syntax.Word, bool) {
	if len(args) == 0 {
		return args, true
	}
	if _, known := staticCommandName(args[0]); known {
		return args, true
	}
	fields, known := constantWordFields(args[0], constants)
	if !known {
		return args, false
	}
	expanded := make([]*syntax.Word, 0, len(fields)+len(args)-1)
	for _, field := range fields {
		expanded = append(expanded, exactShellWord(field, args[0].Pos()))
	}
	expanded = append(expanded, args[1:]...)
	return expanded, len(expanded) > 0
}

func inlineBindingsAt(word *syntax.Word, loops map[string][]string, constants map[string]shellConstant) map[string][]string {
	bindings := make(map[string][]string, len(loops)+len(constants))
	for name, value := range constants {
		if !value.end.After(word.Pos()) {
			bindings[name] = []string{value.value}
		}
	}
	for name, values := range loops {
		bindings[name] = values
	}
	return bindings
}
