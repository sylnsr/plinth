package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"plinth/internal/asklint"
)

// wordsOf is __complete's output with the words only: each line cut at its tab.
func wordsOf(out string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(out, "\n") {
		if w, _, ok := strings.Cut(l, "\t"); ok {
			l = w + "\n"
		}
		b.WriteString(l)
	}
	return b.String()
}

// marked runs __complete on words in a UTF-8 locale and returns every candidate with whether it carries the mark.
func marked(t *testing.T, words ...string) map[string]bool {
	t.Helper()
	t.Setenv("LC_ALL", "C.UTF-8")
	code, out, errs := run(append([]string{CompleteArg}, words...)...)
	if code != exitOK || errs != "" {
		t.Fatalf("%q: %d %q", words, code, errs)
	}
	got := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		w, line, _ := strings.Cut(l, "\t")
		got[w] = strings.HasSuffix(line, " ✓")
	}
	return got
}

// TestMark: ✓ in a UTF-8 locale, + otherwise; the first non-empty of LC_ALL, LC_CTYPE and LANG decides (comment
// 2080).
func TestMark(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"LC_ALL": "C"}, "+"},
		{map[string]string{"LANG": "en_US.UTF-8"}, "✓"},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, "+"},
		{map[string]string{"LC_CTYPE": "C.utf8", "LANG": "C"}, "✓"},
		{map[string]string{"LC_ALL": "", "LANG": "de_DE.utf-8"}, "✓"},
		{map[string]string{}, "+"},
	} {
		if got := mark(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%v: %q, want %q", c.env, got, c.want)
		}
	}
}

// TestPrintCandidates: the ones that fit first, each word once (it fits when any copy does), the listing line in
// columns with the mark; words without a state are printed as they are.
func TestPrintCandidates(t *testing.T) {
	var b bytes.Buffer
	printCandidates(&b, "", []candidate{{"calm-otter", "approved", false}, {"keen-owl", "released", false},
		{"brave-fox-12", "dlp-ok", true}, {"calm-otter", "approved", true}}, "✓")
	want := "brave-fox-12\tbrave-fox-12  (dlp-ok)   ✓\n" +
		"calm-otter\tcalm-otter    (approved) ✓\n" +
		"keen-owl\tkeen-owl      (released)\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	printCandidates(&b, "c", []candidate{{"calm-otter", "approved", false}, {"keen-owl", "released", true}}, "+")
	if b.String() != "calm-otter\tcalm-otter  (approved)\n" {
		t.Errorf("prefix c: %q", b.String())
	}
	b.Reset()
	printCandidates(&b, "", words([]string{"review", "dlp"}), "✓")
	if b.String() != "dlp\nreview\n" {
		t.Errorf("words: %q", b.String())
	}
}

// TestCompletionScript: bash shows the listing lines only on a listing with more than one candidate, and otherwise
// inserts the word only.
func TestCompletionScript(t *testing.T) {
	_, script, _ := run("completion", "bash")
	fake := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(fake, []byte("#!/bin/bash\nif [[ $3 == one ]]; then printf 'calm-otter\\tcalm-otter  (approved) +\\n'; exit; fi\n"+
		"printf 'calm-otter\\tcalm-otter  (approved) +\\nkeen-owl\\tkeen-owl    (ran)\\nexec\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		typ, word, want string
	}{
		{"9", "", "calm-otter|keen-owl|exec|"},
		{"63", "", "calm-otter  (approved) +|keen-owl    (ran)|exec|"},
		{"33", "", "calm-otter  (approved) +|keen-owl    (ran)|exec|"},
		{"63", "one", "calm-otter|"},
		{"37", "", "calm-otter|keen-owl|exec|"},
	} {
		body := script + "\nCOMP_WORDS=(" + fake + " exec " + c.word + "); COMP_CWORD=2; COMP_TYPE=" + c.typ +
			"\n_plinthctl\nprintf '%s|' \"${COMPREPLY[@]}\"\n"
		out, err := exec.Command("bash", "-c", body).CombinedOutput()
		if err != nil || string(out) != c.want {
			t.Errorf("COMP_TYPE %s %q: %v %q, want %q", c.typ, c.word, err, out, c.want)
		}
	}
}

// TestCompletionPlain: finish, read-answer, review and dlp-scan offer every plain AAA run (or its files) with its state,
// and mark the ones the command acts on now.
func TestCompletionPlain(t *testing.T) {
	repo := t.TempDir()
	for f, body := range map[string]string{
		"calm-otter/ask.sh":                      "x",
		"warm-sun/ask.sh":                        "x",
		"warm-sun/answer.new.yml":                "x",
		"cold-sea/ask.sh":                        "x",
		"cold-sea/running.yml":                   "x",
		"done/deep-elm/ask.sh":                   "x",
		"done/deep-elm/answer.dlp-ok.yml":        "x",
		"done/dark-sky/answer.FAIL.DLP-FAIL.yml": "x",
	} {
		p := filepath.Join(repo, "ai", "work", f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{"calm-otter", "cold-sea", "dark-sky", "deep-elm", "warm-sun"}
	check := func(what string, got map[string]bool, offered []string, fit ...string) {
		t.Helper()
		if !slices.Equal(slices.Sorted(maps.Keys(got)), offered) {
			t.Errorf("%s: offered %v, want %v", what, slices.Sorted(maps.Keys(got)), offered)
		}
		for w, m := range got {
			if m != slices.Contains(fit, w) {
				t.Errorf("%s: %s marked %v", what, w, m)
			}
		}
	}
	check("finish", marked(t, "finish", "--repo", repo, ""), all, "warm-sun")
	check("read-answer", marked(t, "read-answer", "--repo", repo, ""), all, "deep-elm")

	t.Setenv("LC_ALL", "C")
	if _, out, _ := run(CompleteArg, "finish", "--repo", repo, "w"); out != "warm-sun\twarm-sun  (new) +\n" {
		t.Errorf("finish w, C locale: %q", out)
	}
	if _, out, _ := run(CompleteArg, "read-answer", "--repo", repo, "da"); out != "dark-sky\tdark-sky  (done, DLP-FAIL, gate)\n" {
		t.Errorf("read-answer da: %q", out)
	}

	t.Chdir(repo)
	files := []string{"ai/work/calm-otter/ask.sh", "ai/work/cold-sea/ask.sh", "ai/work/done/dark-sky/answer.FAIL.DLP-FAIL.yml",
		"ai/work/done/deep-elm/answer.dlp-ok.yml", "ai/work/done/deep-elm/ask.sh", "ai/work/warm-sun/answer.new.yml", "ai/work/warm-sun/ask.sh"}
	check("review", marked(t, "review", "security", ""), files,
		"ai/work/calm-otter/ask.sh", "ai/work/cold-sea/ask.sh", "ai/work/warm-sun/answer.new.yml", "ai/work/warm-sun/ask.sh")
	check("dlp-scan", marked(t, "dlp-scan", ""), []string{"ai/work/done/dark-sky/answer.FAIL.DLP-FAIL.yml",
		"ai/work/done/deep-elm/answer.dlp-ok.yml", "ai/work/warm-sun/answer.new.yml"}, "ai/work/done/dark-sky/answer.FAIL.DLP-FAIL.yml")
	if got := marked(t, "dlp-scan", "./"); len(got) != 0 {
		t.Errorf("dlp-scan ./: %v, want nothing (bash's file names)", got)
	}
}

// TestCompletionMarks: the controller's commands offer every run and mark the ones decide allows now, read as exec does
// (the mode and the stored sum, never ask lint), and the ones get ask, get ans and describe would print (comment
// 2079).
func TestCompletionMarks(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org") // automatic: [security]
	head := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY\nAGENT_NAME=\"devops/main\"\nRUN_NAME=\"x\"\n"
	ok := "## Verdict\nOK to run\n"
	script := func(mode string, lib bool) map[string]string {
		s := head + mode
		if lib {
			s += asklint.PlinthLibLine + "\n"
		}
		sum := sha256.Sum256([]byte(s))
		return map[string]string{"ask.sh": s, "ask.sh.sha256": hex.EncodeToString(sum[:]) + "  ask.sh\n", "review.security.md": ok}
	}
	normal := "INTERACTIVE=0\nNETWORK=0\nPRIVILEGED=0\n"
	network := "INTERACTIVE=0\nNETWORK=1\nPRIVILEGED=0\n"
	// keen-tapir: NETWORK=1, not accepted; brave-fox: accepted; proud-koala: PRIVILEGED=1; eager-lynx: INTERACTIVE=1;
	// odd-wren: changed after its sum; old-wren: fails ask lint, still marked; lost-owl: no ask.sh; bold-bison: a
	// scanned answer; bold-birch: a failed scan; keen-owl: a failed review.
	runs := map[string]map[string]string{
		"calm-otter":  script(normal, true),
		"keen-tapir":  script(network, true),
		"brave-fox":   script(network, true),
		"proud-koala": script("INTERACTIVE=0\nNETWORK=0\nPRIVILEGED=1\n", true),
		"eager-lynx":  script("INTERACTIVE=1\nNETWORK=0\nPRIVILEGED=1\n", true),
		"odd-wren":    script(network, true),
		"old-wren":    script(normal, false),
		"lost-owl":    {"review.security.md": ok},
		"bold-bison":  {"review.security.md": ok, "answer.dlp-ok.yml": "x"},
		"bold-birch":  {"review.security.md": ok, "answer.DLP-FAIL.yml": "x"},
		"keen-owl":    {"ask.sh": "x", "review.security.FAILED.md": "no Ollama"},
	}
	runs["brave-fox"]["accept.network.md"] = "accepted"
	runs["odd-wren"]["ask.sh"] += "echo changed\n"
	for run, files := range runs {
		fixtureRun(t, work, run, files)
	}
	if err := os.WriteFile(filepath.Join(work, "michael", "devops", "config.yml"), []byte("plinth:\n  exec:\n    network: true\n    privileged: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all := slices.Sorted(maps.Keys(runs))
	ns := []string{"--work", work, "-n", "michael/devops"}
	for _, c := range []struct {
		cmd []string
		fit []string
	}{
		{[]string{"exec"}, []string{"brave-fox", "calm-otter", "eager-lynx", "old-wren", "proud-koala"}},
		{[]string{"accept", "network"}, []string{"keen-tapir"}},
		{[]string{"done"}, []string{"bold-bison"}},
		{[]string{"override", "dlp"}, []string{"bold-birch"}},
		{[]string{"override", "review"}, []string{"keen-owl"}},
		{[]string{"get", "ans"}, []string{"bold-bison"}},
		{[]string{"describe"}, all},
		{[]string{"save"}, nil},
	} {
		got := marked(t, append(append(c.cmd, ns...), "")...)
		if !slices.Equal(slices.Sorted(maps.Keys(got)), all) {
			t.Errorf("%v: offered %v, want every run", c.cmd, slices.Sorted(maps.Keys(got)))
		}
		for _, r := range all {
			if got[r] != slices.Contains(c.fit, r) {
				t.Errorf("%v: %s marked %v", c.cmd, r, got[r])
			}
		}
	}
	// The listing: the ones that fit first, each with its state.
	t.Setenv("LC_ALL", "C")
	_, out, _ := run(append(append([]string{CompleteArg, "done"}, ns...), "bold-")...)
	if out != "bold-bison\tbold-bison  (dlp-ok)     +\nbold-birch\tbold-birch  (dlp-failed)\n" {
		t.Errorf("done bold-: %q", out)
	}
}
