package asklint

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// plinthctlRe is plinthctl as a word of its own, so also /aaa/plinthctl (the controller's binary mounted in the run's
	// container, container.BinPath), but not plinthctl.README.md or a name that only contains it.
	plinthctlRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])plinthctl(?:$|[^A-Za-z0-9_.-])`)
	// notPrivilegedRe is the PRIVILEGED=0 line of a script the controller runs.
	notPrivilegedRe = regexp.MustCompile(`(?m)^PRIVILEGED=0(?:\s|$)`)
)

// PlinthctlWarnings returns the lines of script that call plinthctl, though the script runs in Plinth's container and
// can't (a lint warning only, not a security finding). A script with PRIVILEGED=0 and the
// controller's library line runs in the run's container, where plinthctl is not on the PATH and the controller's binary
// at /aaa/plinthctl has no socket to reach the controller; a call fails there. Plain AAA and PRIVILEGED=1 scripts run
// in the user's terminal, where plinthctl works, and get none: whether they reach into Plinth is for the security and
// DLP reviews. Like the layout check it reads words: quoted text, comments and here-document bodies are skipped, so
// `say "ask michael to run plinthctl approve"` is not a call. At most maxWarnings of them, then a line counting the rest.
func PlinthctlWarnings(script []byte) []string {
	text := string(script)
	if !notPrivilegedRe.MatchString(text) || !strings.Contains(text, PlinthLibLine) {
		return nil
	}
	var (
		out     []string
		quote   byte
		heredoc string
	)
	for i, raw := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if heredoc != "" {
			if strings.TrimSpace(raw) == heredoc {
				heredoc = ""
			}
			continue
		}
		wasQuoted := quote != 0
		var s string
		s, quote = scanLine(raw, quote)
		if wasQuoted {
			continue
		}
		if plinthctlRe.MatchString(s) {
			out = append(out, fmt.Sprintf("line %d: plinthctl is not available in the run's container (PRIVILEGED=0), so this call fails", i+1))
		}
		if quote == 0 {
			if m := heredocRe.FindStringSubmatchIndex(raw); m != nil {
				if at := m[0] + strings.Index(raw[m[0]:m[2]], "<<"); at+2 <= len(s) && s[at:at+2] == "<<" {
					heredoc = raw[m[2]:m[3]]
				}
			}
		}
	}
	if len(out) > maxWarnings {
		more := len(out) - maxWarnings
		out = append(out[:maxWarnings:maxWarnings], fmt.Sprintf("and %d more lines", more))
	}
	return out
}
