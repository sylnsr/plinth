package asklint

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"plinth/internal/names"
)

var (
	// issueLineRe is a script's ISSUE= line, whatever follows the =.
	issueLineRe = regexp.MustCompile(`^ISSUE=(.*)$`)
	// issueValueRe is its value: a double-quoted string, then at most a comment.
	issueValueRe = regexp.MustCompile(`^"([^"]*)"[ \t]*(?:#.*)?$`)
	// issueURLRe is an issue's page: <scheme>://<host>/<owner>/<repo>/issues/<N>.
	issueURLRe = regexp.MustCompile(`^(https?://[^/\s"?#]+)/([^/\s"?#]+)/([^/\s"?#]+)/issues/([0-9]{1,9})$`)
)

// Header is the text of line 2, "# AAA script: <what the round does>", trimmed; "" without one.
func Header(script []byte) string {
	lines := strings.SplitN(string(script), "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	m := headerRe.FindStringSubmatch(strings.TrimSuffix(lines[1], "\r"))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// IssueRef is an issue of a Forgejo: the page it has and where it is.
type IssueRef struct {
	URL    string // https://forgejo.example.org/owner/repo/issues/80
	Base   string // https://forgejo.example.org
	Repo   names.Namespace
	Number string
}

// ParseIssueURL reads an issue's page; false when s is not one. A repo or an owner that isn't a name is not one.
func ParseIssueURL(s string) (IssueRef, bool) {
	m := issueURLRe.FindStringSubmatch(s)
	if m == nil {
		return IssueRef{}, false
	}
	repo, err := names.ParseNamespace(m[2] + "/" + m[3])
	if err != nil {
		return IssueRef{}, false
	}
	return IssueRef{URL: s, Base: m[1], Repo: repo, Number: m[4]}, true
}

// OnForgejo reports whether the issue is on the Forgejo at base (forgejo-url): the same scheme and host.
func (i IssueRef) OnForgejo(base string) bool {
	a, errA := url.Parse(i.Base)
	b, errB := url.Parse(base)
	return errA == nil && errB == nil && a.Host != "" && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// issueLines are the script's ISSUE= lines (the line's index and its value after the =).
func issueLines(script []byte) (idx []int, vals []string) {
	for i, l := range strings.Split(string(script), "\n") {
		if m := issueLineRe.FindStringSubmatch(strings.TrimSuffix(l, "\r")); m != nil {
			idx, vals = append(idx, i), append(vals, m[1])
		}
	}
	return idx, vals
}

// ScriptIssue is the issue the script's ISSUE= line names; false without a line, or with a malformed or repeated one
// (Check says what is wrong with those).
func ScriptIssue(script []byte) (IssueRef, bool) {
	_, vals := issueLines(script)
	if len(vals) != 1 {
		return IssueRef{}, false
	}
	m := issueValueRe.FindStringSubmatch(vals[0])
	if m == nil {
		return IssueRef{}, false
	}
	return ParseIssueURL(m[1])
}

// checkIssue is Check's findings about the ISSUE= line, which is optional: at most one, a double-quoted issue
// page and, when forgejoURL is known, on that Forgejo.
func checkIssue(script []byte, forgejoURL string) []string {
	idx, vals := issueLines(script)
	switch {
	case len(vals) == 0:
		return nil
	case len(vals) > 1:
		return []string{fmt.Sprintf("%d ISSUE= lines, want at most 1", len(vals))}
	}
	ref, ok := ScriptIssue(script)
	if !ok {
		return []string{fmt.Sprintf(`line %d: ISSUE= is not ISSUE="https://<forgejo>/<owner>/<repo>/issues/<N>" (the page of the issue this round is for)`, idx[0]+1)}
	}
	if forgejoURL != "" && !ref.OnForgejo(forgejoURL) {
		return []string{fmt.Sprintf("line %d: ISSUE= names %s, which is not on this Plinth's Forgejo (%s)", idx[0]+1, ref.Base, forgejoURL)}
	}
	return nil
}

// WithIssue returns the script with ISSUE="<page>" on the line before the AAA library line, when it has no ISSUE= line;
// a script that has one is returned as it is when the line says the same, and is an error when it says another. A
// script without a library line is returned as it is: Check refuses it anyway.
func WithIssue(script []byte, page string) ([]byte, error) {
	if _, vals := issueLines(script); len(vals) > 0 {
		if ref, ok := ScriptIssue(script); ok && ref.URL == page {
			return script, nil
		}
		return nil, fmt.Errorf("the script says ISSUE=%s, but issue says %s: leave one out or make them the same", vals[0], page)
	}
	lines := strings.Split(string(script), "\n")
	for i, l := range lines {
		if strings.TrimRight(l, " \t\r") == PlinthLibLine {
			out := append(lines[:i:i], `ISSUE="`+page+`"`)
			return []byte(strings.Join(append(out, lines[i:]...), "\n")), nil
		}
	}
	return script, nil
}
