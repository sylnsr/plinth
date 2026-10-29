package asklint

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"plinth/internal/workdir"
)

// scriptOf joins lines under a first line, the way an ask script starts.
func scriptOf(lines ...string) []byte {
	return []byte("#!/bin/bash\n" + strings.Join(lines, "\n") + "\n")
}

func TestWarnings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"flat", []string{"for p in a b; do", "say \"$p\"", "done"}, []string{"line 3: expected 2 spaces, found 0"}},
		{"nested", []string{
			"for p in a b; do",
			"  if [[ -d $p ]]; then",
			"    say yes",
			"  elif [[ -f $p ]]; then",
			"    say file",
			"  else",
			"    say no",
			"  fi",
			"done"}, nil},
		{"if flat inside", []string{"if [[ -d x ]]; then", "say yes", "else", "say no", "fi"},
			[]string{"line 3: expected 2 spaces, found 0", "line 5: expected 2 spaces, found 0"}},
		{"too deep", []string{"say a", "    say b"}, []string{"line 3: expected 0 spaces, found 4"}},
		{"odd indent", []string{"if true; then", "   say a", "fi"}, []string{"line 3: expected 2 spaces, found 3"}},
		{"a tab", []string{"if true; then", "\tsay a", "fi"}, []string{"line 3: a tab in the indent, want spaces (2 per level)"}},
		{"one line if", []string{"if true; then say a; fi", "say b"}, nil},
		{"then on its own line", []string{"if true", "then", "  say a", "fi"}, nil},
		{"pipe broken after |", []string{"uname -a |", "  scrub |", "  block"}, nil},
		{"pipe not indented", []string{"uname -a |", "scrub |", "block"},
			[]string{"line 3: expected 2 spaces, found 0", "line 4: expected 2 spaces, found 0"}},
		{"continuation with a backslash", []string{"ls -l \\", "  -a"}, nil},
		{"case", []string{
			"case \"$x\" in",
			"  a)",
			"    say a",
			"    ;;",
			"  b) say b ;;",
			"  c)",
			"    say c",
			"  ;;",
			"esac",
			"say after"}, nil},
		{"case flat", []string{"case \"$x\" in", "a)", "say a", ";;", "esac"},
			[]string{"line 3: expected 2 spaces, found 0", "line 4: expected 4 spaces, found 0", "line 5: expected 2 spaces, found 0"}},
		{"here-document body is text", []string{"cat <<TXT", "not   indented", "if for fi", "TXT", "say after"}, nil},
		{"quoted here-document", []string{"cat <<-'TXT'", "if", "TXT", "say after"}, nil},
		{"here-string is not a here-document", []string{"grep -q x <<< \"$y\"", "say after"}, nil},
		{"multi-line quote", []string{"x=\"one", "if", "two\"", "say after"}, nil},
		{"keywords in strings and comments", []string{"say \"if for while\"   # then do", "say 'case x in'", "say after"}, nil},
		{"array", []string{"OUT=(", "  one", "  two", ")", "say after"}, nil},
		{"braces", []string{"cmd || {", "  say failed", "}", "say after"}, nil},
		{"subshell on one line", []string{"( cd /x && ls ) 2>&1 | block", "say after"}, nil},
		{"comment lines are not checked", []string{"if true; then", "# anything", "  say a", "fi"}, nil},
	}
	for _, tc := range tests {
		got := layout(scriptOf(tc.in...))
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestWarningsTemplates: the templates, as they are and filled in, have no layout warning.
func TestWarningsTemplates(t *testing.T) {
	for name, tpl := range map[string][]byte{"plinth": workdir.AskTemplate, "aaa": workdir.AAATemplate} {
		if w := Warnings(tpl); len(w) != 0 {
			t.Errorf("the %s template has layout warnings: %q", name, w)
		}
	}
}

func TestWarningsCapped(t *testing.T) {
	var lines []string
	for i := 0; i < 15; i++ {
		lines = append(lines, "if true; then", "say a", "fi")
	}
	w := Warnings(scriptOf(lines...))
	if len(w) != maxWarnings+1 || w[maxWarnings] != fmt.Sprintf("and %d more lines", 15-maxWarnings) {
		t.Errorf("15 warnings: got %d lines, last %q", len(w), w[len(w)-1])
	}
}

// TestWarningsAreNotFindings: Check is the same with or without layout warnings; a flat script still passes it.
func TestWarningsAreNotFindings(t *testing.T) {
	needBash(t)
	flat := filled(t) + "\nfor p in a b; do\nsay \"$p\"\ndone\n"
	if w := Warnings([]byte(flat)); len(w) != 1 {
		t.Errorf("a flat for loop: %q", w)
	}
	if f := Check([]byte(flat), Options{Plinth: true, Work: "/home/u/.work"}); len(f) != 0 {
		t.Errorf("layout is advice, but Check says: %v", f)
	}
}
