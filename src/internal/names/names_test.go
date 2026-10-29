package names

import "testing"

func TestDictionaries(t *testing.T) {
	if len(Adjectives) != 40 || len(Nouns) != 200 {
		t.Fatalf("dictionaries: %d adjectives x %d nouns, want 40 x 200", len(Adjectives), len(Nouns))
	}
	seen := map[string]bool{}
	for _, w := range append(append([]string{}, Adjectives...), Nouns...) {
		if seen[w] {
			t.Errorf("duplicate word %q", w)
		}
		seen[w] = true
		if !ValidRun(w + "-x") {
			t.Errorf("word %q does not make a valid RUN_NAME", w)
		}
	}
}

func TestValidRun(t *testing.T) {
	for _, s := range []string{"calm-otter", "calm-otter-1", "calm-otter-12345"} {
		if !ValidRun(s) {
			t.Errorf("ValidRun(%q) = false", s)
		}
	}
	for _, s := range []string{"", "calm", "calm-", "Calm-otter", "calm-otter-", "calm-otter-x", "calm_otter", "a-b-c", "../calm-otter", "calm-otter/x"} {
		if ValidRun(s) {
			t.Errorf("ValidRun(%q) = true", s)
		}
	}
	if got := Stem("calm-otter-12"); got != "calm-otter" {
		t.Errorf("Stem = %q", got)
	}
	if got := Stem("nope"); got != "" {
		t.Errorf("Stem(nope) = %q", got)
	}
	for run, want := range map[string]string{"calm-otter-12": "12", "calm-otter": "", "calm-otter-1234567890": "", "nope-12x": ""} {
		if got := Issue(run); got != want {
			t.Errorf("Issue(%s) = %q, want %q", run, got, want)
		}
	}
}

func TestParseNamespace(t *testing.T) {
	ns, err := ParseNamespace("michael/devops")
	if err != nil || ns.Owner != "michael" || ns.Repo != "devops" || ns.String() != "michael/devops" {
		t.Fatalf("ParseNamespace = %+v, %v", ns, err)
	}
	for _, s := range []string{"", "michael", "michael/", "/devops", "a/b/c", "../devops", "michael/..", "michael/.", ".x/y", "a b/c"} {
		if _, err := ParseNamespace(s); err == nil {
			t.Errorf("ParseNamespace(%q) accepted", s)
		}
	}
}

func TestValidAgentAndIssue(t *testing.T) {
	if !ValidAgent("main") || !ValidAgent("al") || ValidAgent("Main") || ValidAgent("1a") || ValidAgent("") {
		t.Error("ValidAgent")
	}
	for _, s := range Reserved {
		if ValidAgent(s) {
			t.Errorf("ValidAgent(%q): a reserved name passed", s)
		}
	}
	if !ValidAgent("users") || !ValidAgent("worker-2") {
		t.Error("ValidAgent")
	}
	if !ValidIssue("1") || !ValidIssue("123") || ValidIssue("") || ValidIssue("1a") || ValidIssue("1234567890") {
		t.Error("ValidIssue")
	}
}

func TestDraw(t *testing.T) {
	taken := map[string]bool{}
	for _, a := range Adjectives {
		for _, n := range Nouns {
			taken[a+"-"+n] = true
		}
	}
	delete(taken, "calm-otter")
	got, err := Draw(taken, func(n int) int { return n - 1 })
	if err != nil || got != "calm-otter" {
		t.Fatalf("Draw with one free pair = %q, %v", got, err)
	}
	taken["calm-otter"] = true
	if _, err := Draw(taken, func(int) int { return 0 }); err == nil {
		t.Fatal("Draw with no free pair did not fail")
	}
}
