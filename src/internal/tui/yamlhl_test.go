package tui

import (
	"strings"
	"testing"
)

func TestHighlightYAML(t *testing.T) {
	doc := []string{
		`# top`,
		`---`,
		`result: "ok"`,
		`items:`,
		`  - name: foo # why`,
		`    count: 12`,
		`    on: true`,
		`    note: |`,
		`      text: not a key # nor a comment`,
		`      more`,
		`  - [a, 3, {k: v}]`,
		`x: 'it''s'`,
		`plain: some text here`,
		`  - &a val`,
		`ref: *a`,
		`long: "two`,
		`  lines" # end`,
		`url: http://host:80/p`,
	}
	got := highlightYAML(doc)
	for i, spans := range got {
		var b strings.Builder
		for _, sp := range spans {
			b.WriteString(sp.s)
		}
		if b.String() != doc[i] {
			t.Errorf("line %d: spans join to %q, want %q", i, b.String(), doc[i])
		}
	}
	has := func(line int, k hlKind, s string) bool {
		for _, sp := range got[line] {
			if sp.k == k && strings.Contains(sp.s, s) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		line int
		k    hlKind
		s    string
	}{
		{0, hlComment, "# top"},
		{1, hlKeyword, "---"},
		{2, hlKey, "result"},
		{2, hlOp, ":"},
		{2, hlString, `"ok"`},
		{3, hlKey, "items"},
		{4, hlOp, "-"},
		{4, hlKey, "name"},
		{4, hlString, "foo"},
		{4, hlComment, "# why"},
		{5, hlNumber, "12"},
		{6, hlKeyword, "true"},
		{7, hlKey, "note"},
		{7, hlOp, "|"},
		{8, hlString, "text: not a key # nor a comment"}, // the body of a block scalar is text
		{9, hlString, "more"},
		{10, hlOp, "["},
		{10, hlString, "a"},
		{10, hlNumber, "3"},
		{10, hlKey, "k"},
		{10, hlOp, "}"},
		{11, hlString, `'it''s'`},
		{12, hlKey, "plain"},
		{12, hlString, "some text here"},
		{13, hlKeyword, "&a"},
		{14, hlKeyword, "*a"},
		{15, hlString, `"two`},
		{16, hlString, `lines"`}, // a quote that ran over the line break ends here
		{16, hlComment, "# end"},
		{17, hlString, "http://host:80/p"},
	} {
		if !has(c.line, c.k, c.s) {
			t.Errorf("line %d: no %q of kind %d in %v", c.line, c.s, c.k, got[c.line])
		}
	}
	if has(8, hlKey, "text") || has(8, hlComment, "#") {
		t.Errorf("block scalar body coloured as YAML: %v", got[8])
	}
	if has(17, hlKey, "host") {
		t.Errorf("a colon inside a value made a key: %v", got[17])
	}
}

func TestHighlightFor(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{{"ask.sh", true}, {"answer.dlp-ok.yml", true}, {"x.yaml", true}, {"review.security.md", false}, {"notes.txt", false}} {
		if got := highlight(c.name, []string{"a: b"}) != nil; got != c.want {
			t.Errorf("highlight(%q) coloured: %v, want %v", c.name, got, c.want)
		}
	}
}
