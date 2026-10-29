package asklint

import (
	"fmt"
	"strings"
	"testing"
)

func TestPlinthctlWarnings(t *testing.T) {
	needBash(t)
	plinth := filled(t)
	if !strings.Contains(plinth, "\nPRIVILEGED=0") {
		t.Fatal("the template's PRIVILEGED line changed; update this test")
	}
	privileged := strings.Replace(plinth, "\nPRIVILEGED=0", "\nPRIVILEGED=1", 1)
	tests := []struct {
		name   string
		script string
		add    string
		want   []string
	}{
		{"a call", plinth, "plinthctl status\n", []string{"plinthctl is not available in the run's container (PRIVILEGED=0), so this call fails"}},
		{"the mounted binary", plinth, "/aaa/plinthctl status\n", []string{"plinthctl is not available"}},
		{"in a pipe", plinth, "echo x | plinthctl ask lib\n", []string{"plinthctl is not available"}},
		{"in a command substitution", plinth, "x=$(plinthctl status)\n", []string{"plinthctl is not available"}},
		{"a quoted mention", plinth, "say \"ask michael to run plinthctl approve\"\nsay 'plinthctl approve'\n", nil},
		{"a comment", plinth, "# plinthctl status needs the terminal\n", nil},
		{"a file name", plinth, "cat ai/plinthctl.README.md\n", nil},
		{"a longer name", plinth, "plinthctl-helper x\nmyplinthctl x\n", nil},
		{"a here-document", plinth, "cat <<EOF\nplinthctl status\nEOF\n", nil},
		{"after a here-document", plinth, "cat <<EOF\nx\nEOF\nplinthctl status\n", []string{"plinthctl is not available"}},
		{"a multi-line quote", plinth, "say \"a\nplinthctl b\"\n", nil},
		{"privileged", privileged, "plinthctl status\n", nil},
		{"plain AAA", filledAAA(t), "plinthctl status\n", nil},
		{"no mention", plinth, "uname -a\n", nil},
	}
	for _, c := range tests {
		got := PlinthctlWarnings([]byte(c.script + c.add))
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %d warning(s)", c.name, got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: %q, want it to contain %q", c.name, got[i], w)
			}
		}
	}
	// The line number is the call's.
	n := strings.Count(plinth, "\n") + 2
	if got := PlinthctlWarnings([]byte(plinth + "uname -a\nplinthctl status\n")); len(got) != 1 || !strings.HasPrefix(got[0], fmt.Sprintf("line %d: ", n)) {
		t.Errorf("line number: %q, want line %d", got, n)
	}
}

func TestPlinthctlWarningsCapped(t *testing.T) {
	w := PlinthctlWarnings([]byte(filled(t) + strings.Repeat("plinthctl status\n", 15)))
	if len(w) != maxWarnings+1 || w[maxWarnings] != fmt.Sprintf("and %d more lines", 15-maxWarnings) {
		t.Errorf("15 warnings: got %d lines, last %q", len(w), w[len(w)-1])
	}
}

// TestPlinthctlWarningsAreNotFindings: a call is advice; Check passes the script all the same (the security and DLP
// reviews judge it).
func TestPlinthctlWarningsAreNotFindings(t *testing.T) {
	needBash(t)
	s := filled(t) + "plinthctl status\n"
	if w := PlinthctlWarnings([]byte(s)); len(w) != 1 {
		t.Errorf("a call: %q", w)
	}
	if f := Check([]byte(s), Options{Plinth: true, Work: "/home/u/.work"}); len(f) != 0 {
		t.Errorf("a warning is advice, but Check says: %v", f)
	}
}
