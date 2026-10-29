package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/term"
)

func TestTUINeedsATerminal(t *testing.T) {
	if term.IsTerminal(0) && term.IsTerminal(1) {
		t.Skip("the test runs on a terminal")
	}
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org")
	if code, _, errs := run("tui", "--work", work); code != exitFail || !strings.Contains(errs, "plinthctl tui: needs a terminal") {
		t.Errorf("tui without a terminal: %d %q", code, errs)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"tui", "-n", "../x", "--work", work}, "not <owner>/<repo>"},
		{[]string{"tui", "extra"}, "too many arguments"},
	} {
		if code, _, errs := run(c.args...); code != exitUsage || !strings.Contains(errs, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, errs, c.want)
		}
	}
}
