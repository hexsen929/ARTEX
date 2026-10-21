//go:build ignore

package intercept

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonPrecedingShellEffectsWithoutExecution(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	lib := filepath.Join(root, "lib", "python3.12")
	cwd := filepath.Join(root, "work")
	for _, p := range []string{bin, lib, cwd} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	python := filepath.Join(bin, "python3.12")
	if err := os.WriteFile(python, []byte("\x7fELF fixture metadata"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "one.py"), []byte("KNOWN=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(python, filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, k := range []string{"PYTHONPATH", "PYTHONHOME", "PYTHONUSERBASE", "PYTHONSAFEPATH"} {
		t.Setenv(k, "")
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("tokens.env", "TOKEN=literal\nexport TOKEN\n")
	write("harmful.env", "export PYTHONPATH=/unexpected\n")
	write("shadow.env", "python3(){ echo shadowed; }\n")
	write("nested.env", "source tokens.env\n")
	write("func.env", "H(){ printf '%s' token; }; H\n")
	write("hidden.env", "bash missing.sh\n")
	sentinel := filepath.Join(root, "must-not-exist")
	write("read-only-proof.env", "TOKEN=literal\nprintf '%s' proof > "+sentinel+"\n")
	tests := []struct{ name, prefix, status string }{
		{"helper_function", "H(){ curl -s https://example.test; }; H; ", "complete"},
		{"unused_dangerous_function", "H(){ export PATH=/unknown; }; ", "complete"},
		{"called_environment_mutation", "H(){ export PYTHONPATH=/unknown; }; H; ", "incomplete"},
		{"called_directory_mutation", "H(){ cd /unknown; }; H; ", "incomplete"},
		{"shadowed_directory_builtin", "cd(){ echo nochange; }; cd /unknown; ", "incomplete"},
		{"shadowed_python", "python3(){ echo no interpreter; }; ", "incomplete"},
		{"function_hidden_script", "H(){ bash missing.sh; }; H; ", "incomplete"},
		{"function_literal_inline", "H(){ python3 -c 'print(1)'; }; H; ", "complete"},
		{"function_dynamic_inline", "H(){ python3 -c \"$CODE\"; }; H; ", "incomplete"},
		{"pure_source", "source tokens.env; ", "complete"},
		{"source_path_change", "source harmful.env; ", "incomplete"},
		{"source_interpreter_shadow", "source shadow.env; ", "incomplete"},
		{"nested_source", "source nested.env; ", "incomplete"},
		{"source_function", "source func.env; ", "complete"},
		{"source_hidden_script", "source hidden.env; ", "incomplete"},
		{"source_never_executed", "source read-only-proof.env; ", "complete"},
		{"source_missing", "source absent.env; ", "incomplete"},
		{"dynamic_source", "source \"$UNKNOWN\"; ", "incomplete"},
		{"shell_flags", "set -e; ", "complete"},
		{"relevant_loop_variable", "for PATH in /unknown; do echo harmless; done; ", "incomplete"},
		{"relevant_read", "read PYTHONPATH; ", "incomplete"},
		{"irrelevant_read", "read TOKEN; ", "complete"},
		{"relevant_unset", "unset PYTHONPATH; ", "incomplete"},
		{"irrelevant_unset", "unset TOKEN; ", "complete"},
		{"relevant_export", "export PYTHONPATH; ", "incomplete"},
		{"dynamic_export", "export \"$ASSIGN\"; ", "incomplete"},
		{"nameref_write", "declare -n target=PATH; target=/unknown; ", "incomplete"},
		{"relevant_printf", "printf -v PYTHONPATH '%s' /unknown; ", "incomplete"},
		{"irrelevant_printf", "printf -v TOKEN '%s' known; ", "complete"},
		{"function_recursion", "H(){ H; }; H; ", "incomplete"},
		{"post_module_mutation", "", "complete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := test.prefix + "python3 -m one"
			if test.name == "post_module_mutation" {
				cmd += "; export PYTHONPATH=/later"
			}
			in := reviewCommandForTest(t, cwd, cmd)
			if in.ScriptCaptureStatus != test.status {
				t.Fatalf("status=%s want=%s scripts=%+v", in.ScriptCaptureStatus, test.status, in.Scripts)
			}
		})
	}
	write("too-large.env", strings.Repeat("x", maxReviewScriptBytes+1))
	large := reviewCommandForTest(t, cwd, "source too-large.env; python3 -m one")
	if large.ScriptCaptureStatus != "incomplete" || !large.Scripts[0].Truncated {
		t.Fatalf("oversized frozen source lost its truncation marker: %+v", large)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("source analysis executed code")
	}
	in := reviewCommandForTest(t, cwd, "cat > virtual.env <<'ENV'\nTOKEN=literal\nENV\nsource virtual.env; python3 -m one")
	if in.ScriptCaptureStatus != "complete" || !strings.Contains(in.Scripts[0].Content, "TOKEN=literal") {
		t.Fatalf("virtual source ignored: %+v", in)
	}
}
