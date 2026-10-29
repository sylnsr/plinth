package asklint

import (
	"strings"
	"testing"
)

func TestHeader(t *testing.T) {
	for _, c := range []struct{ script, want string }{
		{"#!/bin/bash\n# AAA script: REVIEW TEST for michael/plinth#80\n# READ-ONLY\n", "REVIEW TEST for michael/plinth#80"},
		{"#!/bin/bash\n# AAA script:   padded  \n", "padded"},
		{"#!/bin/bash\n# not the header\n", ""},
		{"#!/bin/bash\n", ""},
		{"", ""},
	} {
		if got := Header([]byte(c.script)); got != c.want {
			t.Errorf("Header(%q) = %q, want %q", c.script, got, c.want)
		}
	}
}

func TestParseIssueURL(t *testing.T) {
	ref, ok := ParseIssueURL("https://forgejo.example.org/michael/plinth/issues/80")
	if !ok || ref.Repo.String() != "michael/plinth" || ref.Number != "80" || ref.Base != "https://forgejo.example.org" {
		t.Errorf("a page: %+v %v", ref, ok)
	}
	for _, bad := range []string{"", "forgejo.example.org/michael/plinth/issues/80", "https://forgejo.example.org/michael/plinth/pulls/80",
		"https://forgejo.example.org/michael/plinth/issues/", "https://forgejo.example.org/michael/plinth/issues/0123456789",
		"https://forgejo.example.org/michael/plinth/issues/80/", "https://forgejo.example.org/michael/plinth/issues/80?x=1",
		"https://forgejo.example.org/michael/issues/80", "https://forgejo.example.org/a b/plinth/issues/80"} {
		if _, ok := ParseIssueURL(bad); ok {
			t.Errorf("ParseIssueURL(%q) is a page", bad)
		}
	}
	for base, want := range map[string]bool{"https://forgejo.example.org": true, "https://FORGEJO.example.org": true, "https://forgejo.example.org/": true,
		"http://forgejo.example.org": false, "https://other.example.org": false, "": false} {
		if got := ref.OnForgejo(base); got != want {
			t.Errorf("OnForgejo(%q) = %v, want %v", base, got, want)
		}
	}
}

func TestCheckIssue(t *testing.T) {
	needBash(t)
	const page = "https://forgejo.example.org/michael/plinth/issues/80"
	base := filled(t)
	with := func(line string) string {
		return strings.Replace(base, PlinthLibLine, line+"\n"+PlinthLibLine, 1)
	}
	o := Options{Plinth: true, Work: "/home/u/.work", ForgejoURL: "https://forgejo.example.org"}
	for _, c := range []struct{ name, script, want string }{
		{"none", base, ""},
		{"a page", with(`ISSUE="` + page + `"`), ""},
		{"a page and a comment", with(`ISSUE="` + page + `"   # the ticket`), ""},
		{"twice", with(`ISSUE="` + page + `"` + "\n" + `ISSUE="` + page + `"`), "2 ISSUE= lines, want at most 1"},
		{"unquoted", with("ISSUE=" + page), "ISSUE= is not ISSUE="},
		{"not a page", with(`ISSUE="80"`), "ISSUE= is not ISSUE="},
		{"another host", with(`ISSUE="https://evil.example.org/michael/plinth/issues/80"`), "not on this Plinth's Forgejo"},
	} {
		f := Check([]byte(c.script), o)
		switch {
		case c.want == "" && len(f) != 0:
			t.Errorf("%s: findings %v", c.name, f)
		case c.want != "" && !strings.Contains(strings.Join(f, "\n"), c.want):
			t.Errorf("%s: no %q in %v", c.name, c.want, f)
		}
	}
	// Without forgejo-url (ask lint without a config) only the shape is checked.
	if f := Check([]byte(with(`ISSUE="https://evil.example.org/michael/plinth/issues/80"`)), Options{Plinth: true, Work: "/home/u/.work"}); len(f) != 0 {
		t.Errorf("another host, no forgejo-url: %v", f)
	}
}

func TestWithIssue(t *testing.T) {
	const page = "https://forgejo.example.org/michael/plinth/issues/80"
	script := "#!/bin/bash\n# AAA script: x\nNETWORK=0\n" + PlinthLibLine + "\nsay hi\n"
	got, err := WithIssue([]byte(script), page)
	want := "#!/bin/bash\n# AAA script: x\nNETWORK=0\nISSUE=\"" + page + "\"\n" + PlinthLibLine + "\nsay hi\n"
	if err != nil || string(got) != want {
		t.Errorf("inserted: %q, %v", got, err)
	}
	if again, err := WithIssue(got, page); err != nil || string(again) != want {
		t.Errorf("the same page again: %q, %v", again, err)
	}
	if _, err := WithIssue(got, "https://forgejo.example.org/michael/plinth/issues/81"); err == nil || !strings.Contains(err.Error(), "the script says ISSUE=") {
		t.Errorf("another page: %v", err)
	}
	if same, err := WithIssue([]byte("#!/bin/bash\nsay hi\n"), page); err != nil || string(same) != "#!/bin/bash\nsay hi\n" {
		t.Errorf("no library line: %q, %v", same, err)
	}
	if ref, ok := ScriptIssue(got); !ok || ref.URL != page {
		t.Errorf("ScriptIssue: %+v %v", ref, ok)
	}
}
