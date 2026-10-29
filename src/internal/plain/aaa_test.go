package plain

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/config"
	"plinth/internal/doc"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/workdir"
)

// TestMain is plinthctl's text filter when the AAA library (Lib, with this test binary as plinthctl) calls it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == TextArg {
		os.Exit(TextMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// aaaPath is a PATH with a plinthctl that prints the AAA library (ask lib), or with loads false one that prints
// nothing.
func aaaPath(t *testing.T, loads bool) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	lib := filepath.Join(bin, "aaa.bash")
	write(t, lib, string(Lib(self)))
	stub := "#!/bin/bash\nexit 9\n"
	if loads {
		stub = "#!/bin/bash\nif [[ \"$1 $2\" == \"ask lib\" ]]; then exec cat " + shellQuote(lib) + "; fi\nexit 9\n"
	}
	write(t, filepath.Join(bin, "plinthctl"), stub)
	if err := os.Chmod(filepath.Join(bin, "plinthctl"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin + ":" + os.Getenv("PATH")
}

// aaaRun creates a run in dir from the embedded template with steps in place of its example section and returns
// ask.sh and the run's name.
func aaaRun(t *testing.T, dir, steps string) (string, string) {
	t.Helper()
	ask, err := NewAsk(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ask)
	i, j := strings.Index(string(b), "section \"1. <section>\"\n"), strings.Index(string(b), "# GATE example")
	if i < 0 || j < i {
		t.Fatal("the template's example sections are not there to replace")
	}
	s := string(b)[:i] + steps + "\n" + string(b)[j:]
	if err := os.WriteFile(ask, []byte(s), 0o700); err != nil {
		t.Fatal(err)
	}
	return ask, filepath.Base(filepath.Dir(ask))
}

// runAsk runs ask.sh as the user does (by a relative path from ai/work, or by its absolute path) and returns its exit
// code, stdout and stderr.
func runAsk(t *testing.T, path, ask string, relative bool, env ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("bash", ask)
	if relative {
		dir := filepath.Dir(filepath.Dir(ask))
		rel, _ := filepath.Rel(dir, ask)
		cmd = exec.Command("./" + rel)
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + path}, env...)...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func decodeAnswer(t *testing.T, file string) *doc.Answer {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var a doc.Answer
	if err := doc.Decode(b, &a); err != nil {
		t.Fatalf("%s: %v\n%s", file, err, b)
	}
	return &a
}

// TestAAARun: a run from the embedded template writes running.yml through the AAA library only, and its EXIT trap ends
// it as answer.new.yml (a valid document from a script); read-answer won't read that, finish scans it into done/
// as answer.dlp-ok.yml, which read-answer prints; the run never runs again.
func TestAAARun(t *testing.T) {
	path := aaaPath(t, true)
	dir := workDir(t)
	ask, run := aaaRun(t, dir, `say "before any section"
section "1. echo"
printf 'a\tb\x01\n---\nkey: v1\n' | block
say "password=hunter2"
`)
	code, out, errs := runAsk(t, path, ask, true, "AAA_QUIET=1")
	if code != 0 || out != run+" done (ok) - next: the user runs plinthctl finish "+run+", then the agent plinthctl read-answer "+run+"\n" || errs != "" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	newFile := filepath.Join(dir, run, "answer.new.yml")
	a := decodeAnswer(t, newFile)
	if err := a.Check(true); err != nil || a.Metadata.Run != run || a.Result != doc.ResultOK || len(a.Sections) != 2 ||
		a.Sections[0] != (doc.Section{Title: "", Output: "before any section\n"}) ||
		a.Sections[1] != (doc.Section{Title: "1. echo\n", Output: "a\tb\\x01\n---\nkey: v1\npassword=hunter2\n"}) {
		t.Errorf("answer.new.yml: %+v %v", a, err)
	}
	if fi, _ := os.Stat(newFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if r, _ := ReadAnswer(dir, run); r.Code != AnswerNotScanned || !strings.Contains(r.Status, "plinthctl finish "+run) {
		t.Errorf("read-answer before finish: %+v", r)
	}

	s := ollamatest.New(t)
	s.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "key: v1") {
			return "other_secret"
		}
		return ""
	}))
	c := ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
	raw := filepath.Join(t.TempDir(), "raw")
	orig, _ := os.ReadFile(newFile)
	f, err := Finish(context.Background(), c, "m", raw, dir, run)
	if err != nil || f.State != StateDLPOK || f.File != filepath.Join(dir, "done", run, "answer.dlp-ok.yml") || f.DLP.Lines != 1 || f.DLP.Fixed != 1 {
		t.Fatalf("finish: %+v %v", f, err)
	}
	a = decodeAnswer(t, f.File)
	if err := a.Check(false); err != nil || a.Status.DLP.Result != doc.ResultOK || a.Status.DLP.Masked != 1 || a.Status.DLP.Model != "m" ||
		a.Sections[1].Output != "a\tb\\x01\n---\n[line 3 masked by dlp-scan: other_secret]\npassword=<masked>\n" {
		t.Errorf("answer.dlp-ok.yml: %+v %v", a, err)
	}
	if b, _ := os.ReadFile(filepath.Join(raw, run, "answer.new.yml.orig")); string(b) != string(orig) || f.Raw != filepath.Join(raw, run, "answer.new.yml.orig") {
		t.Errorf("the original: %q", b)
	}
	if exists(filepath.Join(dir, run)) || exists(filepath.Join(dir, "done", run, "answer.new.yml")) || !exists(filepath.Join(dir, "done", run, "ask.sh")) {
		t.Error("the run directory wasn't moved whole, or answer.new.yml stayed")
	}
	got, err := ReadAnswer(dir, run)
	if b, _ := os.ReadFile(f.File); err != nil || got.Code != 0 || string(got.Body) != string(b) || got.Failed {
		t.Errorf("read-answer: %+v %v", got, err)
	}
	if code, _, errs := runAsk(t, path, filepath.Join(dir, "done", run, "ask.sh"), false); code != 2 || !strings.Contains(errs, "finished already") {
		t.Errorf("a rerun: %d %q", code, errs)
	}
	if _, err := Finish(context.Background(), c, "m", raw, dir, run); err == nil || !strings.Contains(err.Error(), "finished already") {
		t.Errorf("finish again: %v", err)
	}
}

// TestAAAFail: fail stops the run with its reason (answer.FAIL.new.yml), as does an exit other than 0; finish without
// a model changes nothing; a reply that doesn't fit makes DLP-FAIL; a document that doesn't check makes INVALID.
func TestAAAFail(t *testing.T) {
	path := aaaPath(t, true)
	dir := workDir(t)
	ask, run := aaaRun(t, dir, "section \"1. x\"\nsay hi\nfail \"it broke\"\nsay never\n")
	code, out, _ := runAsk(t, path, ask, false)
	if code != 1 || !strings.Contains(out, "FAIL: it broke - stopped here.") || !strings.HasSuffix(out, run+" done (fail) - next: the user runs plinthctl finish "+run+", then the agent plinthctl read-answer "+run+"\n") ||
		!strings.Contains(out, "## 1. x\nhi\n") {
		t.Errorf("%d %q", code, out)
	}
	a := decodeAnswer(t, filepath.Join(dir, run, "answer.FAIL.new.yml"))
	if err := a.Check(true); err != nil || a.Result != doc.ResultFail || a.Failure != "it broke\n" || len(a.Sections) != 2 || a.Sections[1].Output != "hi\n" {
		t.Errorf("%+v %v", a, err)
	}
	raw := filepath.Join(t.TempDir(), "raw")
	if _, err := Finish(context.Background(), ollama.New(config.Ollama{}), "m", raw, dir, run); err == nil || !strings.Contains(err.Error(), "nothing was changed") ||
		!exists(filepath.Join(dir, run, "answer.FAIL.new.yml")) || exists(raw) {
		t.Errorf("finish without Ollama: %v", err)
	}
	s := ollamatest.New(t)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) { return http.StatusOK, "not json" })
	c := ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
	f, err := Finish(context.Background(), c, "m", raw, dir, run)
	if err != nil || f.State != StateDLPFail || f.File != filepath.Join(dir, "done", run, "answer.FAIL.DLP-FAIL.yml") || !strings.Contains(f.Reason, "expected JSON") {
		t.Fatalf("finish, the reply doesn't fit: %+v %v", f, err)
	}
	if a := decodeAnswer(t, f.File); a.Status.DLP.Result != doc.ResultFail || len(a.Sections) != 1 || a.Sections[0].Title != "1. x\n" {
		t.Errorf("answer.FAIL.DLP-FAIL.yml: %+v", a) // the empty first section is gone
	}
	if r, _ := ReadAnswer(dir, run); r.Code != AnswerDLPFail {
		t.Errorf("read-answer: %+v", r)
	}

	ask, run = aaaRun(t, dir, "section \"1. x\"\nfalse || exit 3\n")
	if code, _, _ := runAsk(t, path, ask, false, "AAA_QUIET=1"); code != 3 {
		t.Errorf("exit 3: %d", code)
	}
	a = decodeAnswer(t, filepath.Join(dir, run, "answer.FAIL.new.yml"))
	if a.Result != doc.ResultFail || a.Failure != "the script exited with status 3, not through fail\n" {
		t.Errorf("exit 3: %+v", a)
	}

	// INVALID: a status from the script, a result that doesn't match the name, no result.
	for _, c := range []struct{ name, body, want string }{
		{"answer.new.yml", answerDocOf("x") + "result: ok\nstatus:\n  dlp:\n    result: ok\n    chunks: 0\n    fixed: 0\n    masked: 0\n", "status"},
		{"answer.new.yml", answerDocOf("x") + "result: fail\n", "doesn't match"},
		{"answer.FAIL.new.yml", answerDocOf("x"), "no result"},
		{"answer.new.yml", "apiVersion: plinth/v1\nkind: Answer\n- x\n", "yaml"},
	} {
		run := "zz-yy" // a name new-ask never draws
		write(t, filepath.Join(dir, run, "ask.sh"), "x")
		write(t, filepath.Join(dir, run, c.name), strings.ReplaceAll(c.body, "@RUN@", run))
		f, err := Finish(context.Background(), ollama.New(config.Ollama{}), "m", raw, dir, run)
		final := strings.Replace(c.name, ".new.", ".INVALID.", 1)
		if err != nil || f.State != StateInvalid || !strings.Contains(f.Reason, c.want) || f.File != filepath.Join(dir, "done", run, final) {
			t.Errorf("%q: %+v %v", c.body, f, err)
		}
		if r, _ := ReadAnswer(dir, run); r.Code != AnswerInvalid {
			t.Errorf("read-answer of INVALID: %+v", r)
		}
		if err := os.RemoveAll(filepath.Join(dir, "done", run)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Finish(context.Background(), c, "m", raw, dir, "no-such"); err == nil || !strings.Contains(err.Error(), "no run directory") {
		t.Errorf("no such run: %v", err)
	}
	write(t, filepath.Join(dir, "qq-ww", "running.yml"), "x")
	if _, err := Finish(context.Background(), c, "m", raw, dir, "qq-ww"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("a running run: %v", err)
	}
}

// answerDocOf is an answer document's head for run @RUN@ with one section saying text.
func answerDocOf(text string) string {
	return "apiVersion: plinth/v1\nkind: Answer\nmetadata:\n  run: \"@RUN@\"\nsections:\n  - title: |2\n      1\n    output: |2\n      " + text + "\n"
}

// TestAAARefuses: the AAA library refuses to start a run (exit 2, nothing written) that is INTERACTIVE under AAA_QUIET,
// whose RUN_NAME isn't its directory's, that doesn't parse, or whose library didn't load.
func TestAAARefuses(t *testing.T) {
	path := aaaPath(t, true)
	dir := workDir(t)
	for _, c := range []struct {
		edit func(s string) string
		path string
		want string
	}{
		{func(s string) string { return strings.Replace(s, "INTERACTIVE=0", "INTERACTIVE=1", 1) }, path, "contradictory parameters"},
		{func(s string) string { return strings.Replace(s, "RUN_NAME=\"", "RUN_NAME=\"x", 1) }, path, "does not match the run directory"},
		{func(s string) string { return s + "if then\n" }, path, "does not parse"},
		{func(s string) string { return s }, aaaPath(t, false), "the AAA library didn't load"},
	} {
		ask, _ := aaaRun(t, dir, "say hi\n")
		b, _ := os.ReadFile(ask)
		if err := os.WriteFile(ask, []byte(c.edit(string(b))), 0o700); err != nil {
			t.Fatal(err)
		}
		code, out, errs := runAsk(t, c.path, ask, true, "AAA_QUIET=1")
		if entries, _ := os.ReadDir(filepath.Dir(ask)); code != 2 || !strings.Contains(errs, c.want) || !strings.Contains(errs, "nothing was run") || out != "" || len(entries) != 1 {
			t.Errorf("%s: %d %q %q, %d entries", c.want, code, out, errs, len(entries))
		}
	}
}

// TestTextMain: the text filter appends to an existing regular file only, and echoes what it read.
func TestTextMain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "running.yml")
	write(t, file, "x:\n")
	var out, errs strings.Builder
	if code := TextMain([]string{"-echo", "-indent", "2", file}, strings.NewReader("a\x1b\n"), &out, &errs); code != 0 || out.String() != "a\x1b\n" {
		t.Errorf("%d %q %q", code, out.String(), errs.String())
	}
	if b, _ := os.ReadFile(file); string(b) != "x:\n  a\\x1b\n" {
		t.Errorf("%q", b)
	}
	link := filepath.Join(filepath.Dir(file), "link.yml")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-indent", "2", link}, {"-indent", "2", file + ".missing"}, {file}, {"-indent", "0", file}} {
		if code := TextMain(args, strings.NewReader("y\n"), &out, &errs); code == 0 {
			t.Errorf("%q: exit 0", args)
		}
	}
	if b, _ := os.ReadFile(file); string(b) != "x:\n  a\\x1b\n" {
		t.Errorf("changed: %q", b)
	}
	if !strings.Contains(string(Lib("/opt/it's/plinthctl")), `_AAA_BIN='/opt/it'\''s/plinthctl'`) {
		t.Error("Lib doesn't quote the binary's path")
	}
	if !strings.Contains(string(workdir.AAATemplate), "source <(plinthctl ask lib)") {
		t.Error("the template doesn't source the library")
	}
}

// TestControllerLib: under the controller (ControllerLib) the library leaves the run's checks to the controller:
// the run directory may have any name (the container's is /aaa/run), the empty running.yml must be the one the
// controller wrote, and its last line says the controller scans the answer. A script that removed running.yml ends
// without an answer: the EXIT trap writes none.
func TestControllerLib(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(t.TempDir(), "lib.bash")
	write(t, lib, string(ControllerLib(self)))
	head := "#!/bin/bash\n# AAA script: check\n# READ-ONLY\nAGENT_NAME=\"devops/main\"\nRUN_NAME=\"keen-tapir\"\nINTERACTIVE=0\nNETWORK=0\nPRIVILEGED=0\n" +
		"source \"$AAA_LIB\"; [[ \"${AAA_LOADED:-}\" == 1 ]] || exit 2\n"
	run := func(steps string) (int, string, string) {
		t.Helper()
		write(t, filepath.Join(dir, "ask.sh"), head+steps)
		cmd := exec.Command("bash", filepath.Join(dir, "ask.sh"))
		cmd.Env = append(os.Environ(), "AAA_LIB="+lib, "AAA_QUIET=1")
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, out.String(), errb.String()
	}
	running := filepath.Join(dir, "running.yml")

	if code, out, errs := run("section \"1. x\"\n"); code != 2 || out != "" || !strings.Contains(errs, "running.yml is not the empty file the controller writes") {
		t.Errorf("without the controller's running.yml: %d %q %q", code, out, errs)
	}
	write(t, running, "")
	code, out, errs := run("section \"1. x\"\nsay hi\n")
	if code != 0 || out != "keen-tapir done (ok) - next: the controller scans the answer; the user releases it (Done), then the agent reads it\n" || errs != "" {
		t.Fatalf("a run: %d %q %q", code, out, errs)
	}
	b, err := os.ReadFile(filepath.Join(dir, "answer.new.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if a, err := doc.ParseEnded(b, "keen-tapir", false); err != nil || len(a.Sections) != 1 || a.Sections[0] != (doc.Section{Title: "1. x\n", Output: "hi\n"}) {
		t.Errorf("answer.new.yml: %+v, %v\n%s", a, err, b)
	}
	if err := os.Remove(filepath.Join(dir, "answer.new.yml")); err != nil {
		t.Fatal(err)
	}
	write(t, running, "")
	if code, _, errs := run("n=running\nrm -f \"$n.yml\"\n"); code != 0 || !strings.Contains(errs, "running.yml is gone - no answer") {
		t.Errorf("a script that removed running.yml: %d %q", code, errs)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.yml")); len(m) != 0 {
		t.Errorf("after a script that removed running.yml: %v", m)
	}
	// A run a signal ended (the controller's stop or timeout) has no answer and may run again: running.yml stays.
	write(t, running, "")
	if code, _, errs := run("section \"1. x\"\nsay partial\nkill -TERM $$\nsay never\n"); code != 143 || !strings.Contains(errs, "ended by SIGTERM - no answer") {
		t.Errorf("a run ended by SIGTERM: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(running); !strings.Contains(string(b), "partial") || strings.Contains(string(b), "result:") {
		t.Errorf("running.yml after SIGTERM: %q", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "answer*")); len(m) != 0 {
		t.Errorf("an answer after SIGTERM: %v", m)
	}
}
