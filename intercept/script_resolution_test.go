package intercept

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewCommandForTest(t *testing.T, dir, command string) ReviewInput {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command})
	in, err := BuildReviewInput(WithReviewWorkingDirectory(t.Context(), dir), "Bash", args)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestScriptResolutionLiteralConstantsAndInterpreterModes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"check.py", "spaced name.py", "unicodeс.py", "literal[1].py"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("print('fixture')\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct{ name, command, status string }{
		{"curl_constant", `C='curl -sk -m 30'; $C https://example.test`, "not_applicable"},
		{"constant_native_options", `C='timeout 3 python3'; $C ./check.py`, "complete"},
		{"constant_interpreter", `C='python3'; $C ./check.py`, "complete"},
		{"constant_script_entry", `C='./check.py'; $C`, "complete"},
		{"constant_path", `D='` + dir + `'; python3 $D/check.py`, "complete"},
		{"quoted_constant_path", `F='spaced name.py'; python3 "$F"`, "complete"},
		{"escaped_path", `python3 spaced\ name.py`, "complete"},
		{"quoted_brackets", `python3 'literal[1].py'`, "complete"},
		{"ansi_path", `python3 $'unicode\321\201.py'`, "complete"},
		{"constant_shell_inline", `C='echo ok'; bash -c "$C"`, "not_applicable"},
		{"no_shell_reparse", `C='./check.py; echo'; $C ignored`, "incomplete"},
		{"used_before_assignment", `$C ./check.py; C=python3`, "incomplete"},
		{"reassigned_constant", `C=python3; C=unknown; $C ./check.py`, "incomplete"},
		{"conditional_constant", `if test yes; then C=python3; fi; $C ./check.py`, "incomplete"},
		{"read_can_change_constant", `C=python3; read C; $C ./check.py`, "incomplete"},
		{"function_can_change_constant", `C=python3; change(){ C=unknown; }; change; $C ./check.py`, "incomplete"},
		{"glob_expansion_unresolved", `F='*.py'; python3 $F`, "incomplete"},
		{"python_version", `echo info; python3 --version`, "not_applicable"},
		{"python_version_short", `python3 -V | head`, "not_applicable"},
		{"node_version", `node -v 2>/dev/null; echo done`, "not_applicable"},
		{"python_help", `python3 --help-env`, "not_applicable"},
		{"script_version_argument", `python3 ./check.py --version`, "complete"},
		{"empty_stdin_heredoc", "python3 - <<'PY'\nPY\n", "not_applicable"},
		{"external_stdin", `cat input | python3 -`, "incomplete"},
		{"gnu_time", `/usr/bin/time -f '%e' python3 ./check.py`, "complete"},
		{"gnu_time_dynamic_format", `for i in 1 2; do /usr/bin/time -f "T${i}=%e" python3 ./check.py; done`, "complete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := reviewCommandForTest(t, dir, test.command)
			if in.ScriptCaptureStatus != test.status {
				t.Fatalf("status=%s want=%s scripts=%+v", in.ScriptCaptureStatus, test.status, in.Scripts)
			}
		})
	}
}

func TestGeneratedRelativeAndAppendedSnapshots(t *testing.T) {
	command := "cat > run.py <<'PY'\nprint('known')\nPY\npython3 run.py"
	in := reviewCommandForTest(t, "", command)
	if in.ScriptCaptureStatus != "complete" || len(in.Scripts) != 1 || in.Scripts[0].Path != "run.py" || in.Scripts[0].Content != "print('known')\n" {
		t.Fatalf("relative inline identity lost: %+v", in)
	}
	in = reviewCommandForTest(t, "", "cat > run.py <<'PY'\nfirst\nPY\ncat >> run.py <<'PY'\nsecond\nPY\npython3 run.py")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[0].Content != "first\nsecond\n" {
		t.Fatalf("append of known generated content missing: %+v", in)
	}
	in = reviewCommandForTest(t, "", command[:strings.LastIndex(command, "python3")]+`cd "$UNKNOWN"; python3 run.py`)
	if in.ScriptCaptureStatus != "incomplete" {
		t.Fatal("reused a relative file after an unknown directory change")
	}
	dir := t.TempDir()
	in = reviewCommandForTest(t, dir, "cat >> new.py <<'PY'\nfirst\nPY\npython3 new.py")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[0].Content != "first\n" {
		t.Fatalf("append creating a new file: %+v", in)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.py")); !os.IsNotExist(err) {
		t.Fatal("snapshot analysis wrote a file")
	}
}

func TestPythonModuleSourceResolutionWithoutExecution(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	library := filepath.Join(root, "lib", "python3.12")
	cwd := filepath.Join(root, "work")
	for _, dir := range []string{bin, library, cwd} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	python := filepath.Join(bin, "python3.12")
	if err := os.WriteFile(python, []byte("\x7fELF not executable test metadata"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(root, "must-not-exist")
	write(filepath.Join(library, "site.py"), "open('"+sentinel+"','w').write('startup hook executed')\n")
	write(filepath.Join(library, "json", "__init__.py"), "STANDARD_INIT=1\n")
	write(filepath.Join(library, "json", "tool.py"), "open('"+sentinel+"','w').write('module executed')\n")
	write(filepath.Join(library, "site-packages", "toolpkg", "__init__.py"), "PACKAGE_INIT=1\n")
	write(filepath.Join(library, "site-packages", "toolpkg", "__main__.py"), "PACKAGE_MAIN=1\n")
	t.Setenv("PYTHONPATH", "")
	t.Setenv("PYTHONHOME", "")
	t.Setenv("PYTHONUSERBASE", "")
	t.Setenv("PYTHONSAFEPATH", "")
	in := reviewCommandForTest(t, cwd, python+" -m json.tool")
	if in.ScriptCaptureStatus != "complete" || len(in.Scripts) != 2 || in.Scripts[1].Path != mustCanonicalPath(t, filepath.Join(library, "json", "tool.py")) {
		t.Fatalf("missing standard module sources: %+v", in)
	}
	in = reviewCommandForTest(t, cwd, python+" -m toolpkg")
	if in.ScriptCaptureStatus != "complete" || len(in.Scripts) != 2 || in.Scripts[1].Content != "PACKAGE_MAIN=1\n" {
		t.Fatalf("missing package entry: %+v", in)
	}
	in = reviewCommandForTest(t, cwd, python+" -S -m toolpkg")
	if in.ScriptCaptureStatus != "incomplete" {
		t.Fatal("ignored -S and searched site-packages")
	}
	write(filepath.Join(cwd, "json", "__init__.py"), "LOCAL_INIT=1\n")
	write(filepath.Join(cwd, "json", "tool.py"), "LOCAL_TOOL=1\n")
	in = reviewCommandForTest(t, cwd, python+" -m json.tool")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[1].Content != "LOCAL_TOOL=1\n" {
		t.Fatalf("ignored cwd module shadowing: %+v", in)
	}
	in = reviewCommandForTest(t, cwd, python+" -W -I -m json.tool")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[1].Content != "LOCAL_TOOL=1\n" {
		t.Fatalf("warning option value mistaken for isolation flag: %+v", in)
	}
	in = reviewCommandForTest(t, cwd, python+" -I -m json.tool")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[1].Content == "LOCAL_TOOL=1\n" {
		t.Fatal("isolated Python searched cwd")
	}
	missingDir := filepath.Join(root, "shadow")
	if err := os.MkdirAll(missingDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(missingDir, "json.py"), "NOT_A_PACKAGE=1\n")
	in = reviewCommandForTest(t, missingDir, python+" -m json.tool")
	if in.ScriptCaptureStatus != "incomplete" {
		t.Fatal("substituted stdlib despite a shadowing non-package module")
	}
	extra := filepath.Join(root, "extra")
	write(filepath.Join(extra, "one.py"), "EXTRA=1\n")
	in = reviewCommandForTest(t, cwd, "PYTHONPATH="+extra+" "+python+" -m one")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[0].Content != "EXTRA=1\n" {
		t.Fatalf("ignored explicit PYTHONPATH: %+v", in)
	}
	in = reviewCommandForTest(t, cwd, "PYTHONPATH="+extra+" "+python+" -E -m one")
	if in.ScriptCaptureStatus != "incomplete" {
		t.Fatal("ignored -E environment isolation")
	}
	for _, command := range []string{
		"env -i " + python + " -m json.tool",
		"C='env -i " + python + "'; $C -m json.tool",
		"env -u PYTHONPATH " + python + " -m json.tool",
		"PYTHONPATH=" + extra + "; export PYTHONPATH; " + python + " -m json.tool",
		"PYTHONPATH=" + extra + "; " + python + " -m json.tool",
		"eval 'export PYTHONPATH=unknown'; " + python + " -m json.tool",
	} {
		if in := reviewCommandForTest(t, cwd, command); in.ScriptCaptureStatus != "incomplete" {
			t.Fatalf("guessed a module after an environment change: %+v", in)
		}
	}
	in = reviewCommandForTest(t, cwd, "cat > generated.py <<'PY'\nGENERATED=1\nPY\n"+python+" -m generated")
	if in.ScriptCaptureStatus != "complete" || in.Scripts[0].Source != "command" || in.Scripts[0].Content != "GENERATED=1\n" {
		t.Fatalf("ignored generated local module: %+v", in)
	}
	in = reviewCommandForTest(t, "", python+" -m json.tool")
	if in.ScriptCaptureStatus != "incomplete" {
		t.Fatal("guessed module location with unknown cwd")
	}
	in = reviewCommandForTest(t, "", python+" -I -m json.tool")
	if in.ScriptCaptureStatus != "complete" {
		t.Fatalf("isolated stdlib does not require cwd: %+v", in)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("module resolution executed code or startup hooks")
	}
}

func mustCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGeneratedPipelineSnapshots(t *testing.T) {
	dir := t.TempDir()
	for _, prefix := range []string{"", "cd " + dir + " && "} {
		in := reviewCommandForTest(t, dir, prefix+"cat > run.py <<'PY' 2>&1 | tee run.log\nprint('known')\nPY\npython3 run.py")
		if in.ScriptCaptureStatus != "complete" || len(in.Scripts) != 1 || in.Scripts[0].Content != "print('known')\n" {
			t.Fatalf("literal pipeline producer lost: %+v", in)
		}
	}
	for _, command := range []string{
		"cat > run.py 2>&1 | tee run.log <<'PY'\nprint('consumer input')\nPY\npython3 run.py",
		"cat > run.py <<'PY' 2>&1 | tee run.log &\nprint('background')\nPY\npython3 run.py",
		"cat > run.py <<'PY' >&2 | tee run.log\nprint('wrong output')\nPY\npython3 run.py",
	} {
		if in := reviewCommandForTest(t, dir, command); in.ScriptCaptureStatus != "incomplete" {
			t.Fatalf("opaque pipeline accepted: %+v", in)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "run.py")); !os.IsNotExist(err) {
		t.Fatal("pipeline analysis executed a writer")
	}
}
