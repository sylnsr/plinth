package tui

import (
	"strings"
	"testing"
)

func TestHighlightBash(t *testing.T) {
	script := []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`if [[ -n "$HOME/x" ]]; then echo 'a # not a comment' | wc -l # real comment`,
		`fi`,
		`cat <<-EOF > out`,
		`  $notexpanded "quote`,
		"\tEOF",
		`rm -rf "${DIR}"/$(pwd) \#x`,
		`echo "two`,
		`lines" done`,
	}
	got := highlightBash(script)
	for i, spans := range got {
		var b strings.Builder
		for _, sp := range spans {
			b.WriteString(sp.s)
		}
		if b.String() != script[i] {
			t.Errorf("line %d: spans join to %q, want %q", i, b.String(), script[i])
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
		{0, hlComment, "#!/usr/bin/env bash"},
		{1, hlKeyword, "set"},
		{2, hlKeyword, "if"},
		{2, hlKeyword, "then"},
		{2, hlVar, "$HOME"},
		{2, hlString, "'a # not a comment'"},
		{2, hlOp, "|"},
		{2, hlComment, "# real comment"},
		{3, hlKeyword, "fi"},
		{4, hlOp, "<<-"},
		{5, hlString, "$notexpanded"}, // in a here-document nothing is a variable
		{6, hlKeyword, "EOF"},         // the tab-indented end of <<-
		{7, hlVar, "${DIR}"},
		{7, hlVar, "$("},
		{8, hlString, `"two`},
		{9, hlString, `lines"`},
		{9, hlKeyword, "done"}, // a quote that ran over the line break ends, and the script goes on
	} {
		if !has(c.line, c.k, c.s) {
			t.Errorf("line %d: no %q of kind %d in %v", c.line, c.s, c.k, got[c.line])
		}
	}
	if has(7, hlKeyword, "x") || has(2, hlKeyword, "echo") {
		t.Errorf("a word that is no keyword was taken for one: %v", got[7])
	}
}
