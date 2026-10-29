package asklint

import (
	"fmt"
	"regexp"
	"strings"
)

// IndentWidth is the indent of one level in an ask script (2 spaces only, no tabs).
const IndentWidth = 2

// maxWarnings is the most layout warnings Warnings returns before it counts the rest.
const maxWarnings = 10

var (
	// segRe splits a line (its quotes and comment blanked) into the commands it holds.
	segRe = regexp.MustCompile(`;;|&&|\|\||[;|&]`)
	// heredocRe is a here-document's start: << or <<- and the delimiter, quoted or not. Not <<<, a here-string.
	heredocRe = regexp.MustCompile(`(?:^|[^<])<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)`)
	// casePatRe is a case pattern with its commands on the next lines; casePatStartRe one with the commands after it.
	casePatRe      = regexp.MustCompile(`^[^\s()]+\)\s*$`)
	casePatStartRe = regexp.MustCompile(`^[^\s()]+\)`)

	opens  = map[string]bool{"if": true, "for": true, "while": true, "until": true, "case": true, "select": true, "{": true}
	closes = map[string]bool{"fi": true, "done": true, "esac": true, "}": true}
	// dedent are the words that start a line one level out: a closer, the else of an if, a then or do on a line of its own.
	dedent = map[string]bool{"fi": true, "done": true, "esac": true, "}": true, "else": true, "elif": true, "then": true, "do": true}
)

// Warnings returns the layout findings of script, at most maxWarnings of them and then a line counting the rest; none
// means the indentation is as the templates show it. They are advice, not findings of Check: the script still
// passes, since the user reads it before it runs. Every line reads "line N: expected X spaces, found Y".
func Warnings(script []byte) []string {
	w := layout(script)
	if len(w) > maxWarnings {
		more := len(w) - maxWarnings
		w = append(w[:maxWarnings:maxWarnings], fmt.Sprintf("and %d more lines", more))
	}
	return w
}

// layout checks the indent of every line of script against the block it is in: a line after if/for/while/until/case/
// select/{ (or an unclosed parenthesis) sits one level (IndentWidth) deeper, fi/done/esac/}/) and else/elif one level
// out, a case pattern at the level of the case body's pattern list and its commands one deeper, and a line after a
// trailing \, | or && two deeper than the line that began the command. Like the ssh check it reads words, not shell
// syntax: quoted text and comments are blanked first, here-document bodies and the lines inside a multi-line quote are
// skipped, and comment-only and blank lines are not checked.
func layout(script []byte) []string {
	var out []string
	var (
		depth, cont int
		quote       byte
		heredoc     string
		prevCont    bool
	)
	for i, raw := range strings.Split(strings.TrimSuffix(string(script), "\n"), "\n") {
		no := i + 1
		if heredoc != "" {
			if strings.TrimSpace(raw) == heredoc {
				heredoc = ""
			}
			continue
		}
		wasQuoted := quote != 0
		var s string
		s, quote = scanLine(raw, quote)
		body := strings.TrimLeft(raw, " ")
		if wasQuoted || no == 1 || strings.TrimSpace(raw) == "" || strings.HasPrefix(body, "#") {
			continue
		}
		indent := len(raw) - len(body)
		if strings.HasPrefix(body, "\t") {
			out = append(out, fmt.Sprintf("line %d: a tab in the indent, want spaces (%d per level)", no, IndentWidth))
			continue
		}
		parts := segRe.Split(s, -1)
		first := ""
		if w := strings.Fields(parts[0]); len(w) > 0 {
			first = w[0]
			if first == "!" && len(w) > 1 {
				first = w[1]
			}
		}
		t := strings.TrimSpace(s)
		expect := depth * IndentWidth
		switch {
		case prevCont:
			expect = cont + IndentWidth
		case dedent[first] || strings.HasPrefix(t, ")") || strings.HasPrefix(t, ";;"):
			expect = (depth - 1) * IndentWidth
		}
		if expect < 0 {
			expect = 0
		}
		// The ;; of a case pattern may stand with its commands or with the pattern.
		if indent != expect && !(strings.HasPrefix(t, ";;") && indent == expect+IndentWidth) {
			out = append(out, fmt.Sprintf("line %d: expected %d spaces, found %d", no, expect, indent))
		}
		if !prevCont {
			cont = indent
		}

		delta := 0
		for _, p := range parts {
			w := strings.Fields(p)
			if len(w) > 1 && w[0] == "!" {
				w = w[1:]
			}
			switch {
			case len(w) == 0:
			case opens[w[0]]:
				delta++
			case closes[w[0]]:
				delta--
			}
		}
		if depth > 0 && casePatRe.MatchString(t) {
			delta++ // the commands of a case pattern, on the next lines
		}
		if strings.HasPrefix(t, ";;") {
			delta--
		}
		paren := strings.Count(s, "(") - strings.Count(s, ")")
		if casePatStartRe.MatchString(t) {
			paren++ // a case pattern's own )
		}
		depth = max(0, depth+delta+paren)
		prevCont = strings.HasSuffix(t, `\`) || strings.HasSuffix(t, "|") || strings.HasSuffix(t, "&&")

		if quote == 0 {
			if m := heredocRe.FindStringSubmatchIndex(raw); m != nil {
				// The << must be code, not text in a string: the blanked line keeps it.
				at := m[0] + strings.Index(raw[m[0]:m[2]], "<<")
				if at+2 <= len(s) && s[at:at+2] == "<<" {
					heredoc = raw[m[2]:m[3]]
				}
			}
		}
	}
	return out
}

// scanLine blanks the quoted text and the comment of line, and returns it with every other byte at its column, and the
// quote that is still open at its end (0 for none): the quote carries over to the next line.
func scanLine(line string, quote byte) (string, byte) {
	out := []byte(line)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			out[i] = ' '
		case quote == '"':
			if c == '\\' && i+1 < len(line) {
				out[i], out[i+1] = ' ', ' '
				i++
				continue
			}
			if c == '"' {
				quote = 0
			}
			out[i] = ' '
		case c == '\\':
			if i+1 < len(line) { // a \ at the end of the line is a continuation: it stays
				out[i], out[i+1] = ' ', ' '
				i++
			}
		case c == '\'' || c == '"':
			quote = c
			out[i] = ' '
		case c == '#' && (i == 0 || strings.IndexByte(" \t;|&(", line[i-1]) >= 0):
			return string(out[:i]), quote
		}
	}
	return string(out), quote
}
