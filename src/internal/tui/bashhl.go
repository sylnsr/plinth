package tui

import (
	"strings"

	"plinth/internal/term"
)

// hlKind is what a stretch of a shell script or a YAML file is, for the colour the pager gives it
// (plinth-tui.README.md, Pager). hlKey is a YAML mapping key, hlNumber a YAML number.
type hlKind uint8

const (
	hlPlain hlKind = iota
	hlComment
	hlString  // '...', "...", and the body of a here-document
	hlVar     // $name, ${...}, $(
	hlKeyword // if, then, for, ...
	hlOp      // | & ; < > ( ) and the << of a here-document
	hlKey
	hlNumber
)

// hlSpan is a stretch of one line.
type hlSpan struct {
	s string
	k hlKind
}

// stringColor is a light blue: the 8-colour blue is hard to read on a dark ground, and green means success elsewhere in
// the TUI.
var stringColor = term.RGB(0x61afef)

var hlStyles = map[hlKind]term.Style{
	hlPlain:   normal,
	hlComment: dimStyle,
	hlString:  {FG: stringColor},
	hlVar:     {FG: term.Cyan},
	hlKeyword: {FG: term.Magenta, Bold: true},
	hlOp:      {FG: msgColor},
	hlKey:     {FG: term.Cyan},
	hlNumber:  {FG: term.Magenta},
}

var bashKeywords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true, "for": true, "while": true, "until": true,
	"do": true, "done": true, "case": true, "esac": true, "in": true, "function": true, "select": true, "time": true,
	"return": true, "exit": true, "break": true, "continue": true, "local": true, "export": true, "readonly": true,
	"declare": true, "set": true, "unset": true, "trap": true, "eval": true, "exec": true, "source": true,
}

// highlightBash splits each line of a shell script into spans of one kind. It is a reading aid, not a parser: quotes and
// here-documents carry over from line to line, and nothing is ever dropped or changed, so the spans of a line always
// join to the line.
func highlightBash(lines []string) [][]hlSpan {
	h := &bashLexer{}
	out := make([][]hlSpan, len(lines))
	for i, l := range lines {
		out[i] = h.line(l)
	}
	return out
}

type bashLexer struct {
	spans    []hlSpan
	quote    byte   // ' or " while a quoted string runs over a line break
	heredoc  string // the delimiter of the here-document being read
	stripTab bool   // <<- : the delimiter may be indented with tabs
}

func (h *bashLexer) add(s string, k hlKind) { h.spans = addSpan(h.spans, s, k) }

// addSpan appends s to spans; next to a span of the same kind it joins it.
func addSpan(spans []hlSpan, s string, k hlKind) []hlSpan {
	if s == "" {
		return spans
	}
	if n := len(spans); n > 0 && spans[n-1].k == k {
		spans[n-1].s += s
		return spans
	}
	return append(spans, hlSpan{s, k})
}

func (h *bashLexer) line(l string) []hlSpan {
	h.spans = nil
	if h.heredoc != "" {
		end := l
		if h.stripTab {
			end = strings.TrimLeft(l, "\t")
		}
		if end == h.heredoc {
			h.heredoc = ""
			h.add(l, hlKeyword)
		} else {
			h.add(l, hlString)
		}
		return h.spans
	}
	i := 0
	if h.quote != 0 {
		i = h.quoted(l, 0)
	}
	for i < len(l) {
		c := l[i]
		switch {
		case c == '#' && (i == 0 || strings.IndexByte(" \t;&|(", l[i-1]) >= 0):
			h.add(l[i:], hlComment)
			return h.spans
		case c == '\\':
			end := min(i+2, len(l))
			h.add(l[i:end], hlPlain)
			i = end
		case c == '\'' || c == '"':
			h.quote = c
			h.add(string(c), hlString)
			i = h.quoted(l, i+1)
		case c == '$':
			i = h.dollar(l, i)
		case strings.HasPrefix(l[i:], "<<") && !strings.HasPrefix(l[i:], "<<<"):
			i = h.openHeredoc(l, i)
		case strings.IndexByte("|&;<>(){}", c) >= 0:
			h.add(string(c), hlOp)
			i++
		case isWordByte(c):
			j := i
			for j < len(l) && isWordByte(l[j]) {
				j++
			}
			k := hlPlain
			if bashKeywords[l[i:j]] && (i == 0 || strings.IndexByte(" \t;&|(){", l[i-1]) >= 0) {
				k = hlKeyword
			}
			h.add(l[i:j], k)
			i = j
		default:
			h.add(l[i:i+1], hlPlain)
			i++
		}
	}
	return h.spans
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// quoted adds the part of l from i that is inside the open quote, up to and with the closing one, and returns where it
// stopped. Inside double quotes a $ still shows.
func (h *bashLexer) quoted(l string, i int) int {
	start := i
	for i < len(l) {
		switch c := l[i]; {
		case c == h.quote:
			h.add(l[start:i+1], hlString)
			h.quote = 0
			return i + 1
		case c == '\\' && h.quote == '"':
			i = min(i+2, len(l))
		case c == '$' && h.quote == '"':
			h.add(l[start:i], hlString)
			i = h.dollar(l, i)
			start = i
		default:
			i++
		}
	}
	h.add(l[start:], hlString)
	return len(l)
}

// dollar adds the expansion at l[i] ($name, ${...}, $(, $1, $@) and returns the index after it.
func (h *bashLexer) dollar(l string, i int) int {
	j := i + 1
	switch {
	case j >= len(l):
	case l[j] == '{':
		if e := strings.IndexByte(l[j:], '}'); e >= 0 {
			j += e + 1
		} else {
			j = len(l)
		}
	case l[j] == '(':
		j++
		for j < len(l) && l[j] == '(' {
			j++
		}
	case isWordByte(l[j]):
		for j < len(l) && isWordByte(l[j]) {
			j++
		}
	case strings.IndexByte("@*#?$!-", l[j]) >= 0:
		j++
	}
	h.add(l[i:j], hlVar)
	return j
}

// openHeredoc adds the << (or <<-) and its delimiter at l[i:] and, when there is a delimiter, starts the here-document
// from the next line.
func (h *bashLexer) openHeredoc(l string, i int) int {
	j := i + 2
	strip := j < len(l) && l[j] == '-'
	if strip {
		j++
	}
	for j < len(l) && (l[j] == ' ' || l[j] == '\t') {
		j++
	}
	start := j
	for j < len(l) && (isWordByte(l[j]) || l[j] == '\'' || l[j] == '"' || l[j] == '-' || l[j] == '.') {
		j++
	}
	h.add(l[i:start], hlOp)
	h.add(l[start:j], hlString)
	if d := strings.Trim(l[start:j], `'"`); d != "" {
		h.heredoc, h.stripTab = d, strip
	}
	return j
}

// styledLine is the spans drawn in the colours of its spans. The text still goes through the sanitizer (Untrusted).
func styledLine(spans []hlSpan) *term.Line {
	l := &term.Line{}
	for _, sp := range spans {
		l.Untrusted(sp.s, hlStyles[sp.k])
	}
	return l
}
