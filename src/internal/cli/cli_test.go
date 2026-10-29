package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/doc"
	"plinth/internal/plain"
	"plinth/internal/workdir"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestHelpGroups(t *testing.T) {
	code, out, _ := run("help")
	if code != exitOK {
		t.Fatalf("help exit %d", code)
	}
	plainAt := strings.Index(out, "No backend (plain AAA)")
	ollamaAt := strings.Index(out, "Needs Ollama")
	controllerAt := strings.Index(out, "Needs the controller and Forgejo")
	if plainAt < 0 || ollamaAt < plainAt || controllerAt < ollamaAt {
		t.Fatalf("groups missing or out of order:\n%s", out)
	}
	// Every command is listed under its own group, at the start of a line (a summary may hold a command's name: "stop
	// the Ollama gateway").
	for _, c := range commands() {
		at := strings.Index(out, "\n  "+c.name+" ")
		if at < 0 {
			t.Errorf("%q not listed", c.name)
			continue
		}
		var from, to int
		switch c.group {
		case groupPlain:
			from, to = plainAt, ollamaAt
		case groupOllama:
			from, to = ollamaAt, controllerAt
		case groupController:
			from, to = controllerAt, len(out)
		}
		if at < from || at > to {
			t.Errorf("%q listed outside its group", c.name)
		}
	}
}

func TestHelpCommand(t *testing.T) {
	for _, args := range [][]string{{"help", "new-ask"}, {"new-ask", "--help"}, {"help", "ask", "lint"}, {"agent", "token-remove", "--help"},
		{"agent", "token-rotate", "--help"}, {"register", "--help"}, {"get", "answer", "--help"}, {"help", "get", "answer"}} {
		code, out, _ := run(args...)
		if code != exitOK || !strings.HasPrefix(out, "Usage: plinthctl ") || !strings.Contains(out, "Needs: ") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	if code, out, _ := run("help", "new-ask"); !strings.Contains(out, "Needs: no backend") || code != exitOK {
		t.Errorf("new-ask help does not say what it needs:\n%s", out)
	}
	if _, out, _ := run("help", "get", "answer"); !strings.HasPrefix(out, "Usage: plinthctl get ans ") {
		t.Errorf("get answer is not get ans:\n%s", out)
	}
	for _, args := range [][]string{{"help", "nope"}, {"help", "sync-issues"}} {
		if code, _, _ := run(args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestDispatch(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"sync-issues", "x"}} {
		if code, _, errs := run(args...); code != exitUsage || !strings.Contains(errs, "unknown command") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if code, out, errs := run(); code != exitUsage || out != "" || !strings.Contains(errs, "No backend (plain AAA)") {
		t.Errorf("no args: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if code, out, _ := run("version"); code != exitOK || !strings.HasPrefix(out, "plinthctl ") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, errs := run("approve", "m1"); code != exitFail || !strings.Contains(errs, "not implemented") {
		t.Errorf("approve: %d %q", code, errs)
	}
	if code, _, errs := run("stop"); code != exitUsage || !strings.Contains(errs, "unknown command") {
		t.Errorf("stop, now controller stop: %d %q", code, errs)
	}
	for _, args := range [][]string{{"new-ask", "--bogus"}, {"run-namer", "--dir", "x"}} {
		if code, _, _ := run(args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestPlainCommands(t *testing.T) {
	repo := t.TempDir()
	work := filepath.Join(repo, "ai", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}

	plinthWork := t.TempDir()
	code, out, errs := run("new-ask", "--repo", repo, "--work", plinthWork, "5")
	out = strings.TrimSpace(out)
	if code != exitOK || !strings.HasPrefix(out, work+string(filepath.Separator)) || !strings.HasSuffix(out, "-5/ask.sh") {
		t.Fatalf("new-ask: %d %q %q", code, out, errs)
	}
	name := filepath.Base(filepath.Dir(out))
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "RUN_NAME=\""+name+"\"   # filled in by plinthctl new-ask") ||
		!strings.Contains(string(b), "source <(plinthctl ask lib)") {
		t.Errorf("new-ask did not start from the embedded template:\n%s", b)
	}
	if code, out, _ := run("read-answer", "--repo", repo, name); code != plain.AnswerNone || !strings.HasPrefix(out, name+": not run yet") {
		t.Errorf("read-answer of a queued run: %d %q", code, out)
	}
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: name}, Sections: []doc.Section{{Title: "1. x\n"}},
		Failure: "x\n", Result: doc.ResultFail, Status: &doc.AnswerStatus{DLP: &doc.DLP{Result: doc.ResultOK, Model: "m"}}}
	answer := string(a.Encode())
	if err := os.WriteFile(filepath.Join(work, name, "answer.FAIL.dlp-ok.yml"), []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run("read-answer", "--repo", repo, name); code != exitOK || out != answer || !strings.Contains(errs, "FAILED at a gate") {
		t.Errorf("read-answer: %d %q %q", code, out, errs)
	}
	if code, out, _ := run("ask", "template"); code != exitOK || out != string(workdir.BaseTemplate(doc.ModeAAA)) {
		t.Errorf("ask template: %d", code)
	}
	if code, out, _ := run("ask", "template", "--plinth"); code != exitOK || out != string(workdir.BaseTemplate(doc.ModePlinth)) {
		t.Errorf("ask template --plinth: %d", code)
	}
	if code, out, _ := run("help", "aaa"); code != exitOK || !strings.Contains(out, "plinthctl read-answer <RUN>") || !strings.Contains(out, "plinthctl finish <RUN>") ||
		!strings.Contains(out, "WHAT THE AGENT DOES NEXT") {
		t.Errorf("help aaa: %d %q", code, out)
	}
	if code, out, _ := run("ask", "lib"); code != exitOK || !strings.Contains(out, "AAA_LOADED=1") || !strings.Contains(out, plain.TextArg) {
		t.Errorf("ask lib: %d", code)
	}
	if code, _, errs := run("finish", "--repo", repo, "--work", plinthWork, name); code != exitFail || !strings.Contains(errs, "nothing to finish") {
		t.Errorf("finish of a run without answer.new.yml: %d %q", code, errs)
	}
	if code, _, _ := run(plain.TextArg); code != exitUsage {
		t.Errorf("%s without arguments: %d", plain.TextArg, code)
	}
	if code, out, _ := run("run-namer", "--repo", repo); code != exitOK || strings.TrimSpace(out) == "" {
		t.Errorf("run-namer: %d %q", code, out)
	}
	if code, _, _ := run("run-namer", "--repo", repo, "x1"); code != exitFail {
		t.Errorf("run-namer bad issue: exit %d", code)
	}
	if code, _, errs := run("new-ask", "--repo", t.TempDir()); code != exitFail || errs == "" {
		t.Errorf("new-ask in a repo without ai/work: %d %q", code, errs)
	}

	// --repo defaults to the current directory.
	t.Chdir(repo)
	if code, out, errs := run("new-ask", "--work", plinthWork); code != exitOK || !strings.HasPrefix(out, filepath.Join("ai", "work")+string(filepath.Separator)) {
		t.Errorf("new-ask in the current directory: %d %q %q", code, out, errs)
	}
}
