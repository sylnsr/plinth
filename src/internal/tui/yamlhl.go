package tui

import (
	"strconv"
	"strings"

	"plinth/internal/runstate"
)

// highlight is the colours of the lines of the file name, or nil for a file that stays plain: ask.sh is shell, *.yml and
// *.yaml are YAML.
func highlight(name string, lines []string) [][]hlSpan {
	switch {
	case name == runstate.AskFile:
		return highlightBash(lines)
	case strings.HasSuffix(name, ".yml"), strings.HasSuffix(name, ".yaml"):
		return highlightYAML(lines)
	}
	return nil
}

// highlightYAML splits each line of a YAML file into spans of one kind. As with highlightBash it is a reading aid, not
// a parser: a quoted scalar and a | or > block scalar carry over from line to line, nothing is dropped or changed, and
// the spans of a line always join to the line.
func highlightYAML(lines []string) [][]hlSpan {
	y := &yamlLexer{}
	out := make([][]hlSpan, len(lines))
	for i, l := range lines {
		out[i] = y.line(l)
	}
	return out
}

// yamlLexer holds what runs over a line break: quote is the open quote of a scalar, block that the lines indented more
// than parent belong to a block scalar, flow the depth in [ ] and { } (within a line: a flow collection that runs over a
// line break isn't followed).
type yamlLexer struct {
	spans  []hlSpan
	quote  byte
	block  bool
	parent int
	flow   int
}

func (y *yamlLexer) add(s string, k hlKind) { y.spans = addSpan(y.spans, s, k) }

func (y *yamlLexer) line(l string) []hlSpan {
	y.spans = nil
	y.flow = 0 // a [ or { that never closes must not colour the rest of the file
	if y.quote != 0 {
		y.value(l[y.quoted(l, 0):])
		return y.spans
	}
	indent := len(l) - len(strings.TrimLeft(l, " "))
	if y.block {
		if strings.TrimSpace(l) == "" || indent > y.parent {
			y.add(l, hlString)
			return y.spans
		}
		y.block = false
	}
	y.parent = indent
	y.add(l[:indent], hlPlain)
	rest := l[indent:]
	if indent == 0 && (strings.HasPrefix(rest, "---") || strings.HasPrefix(rest, "...")) &&
		(len(rest) == 3 || rest[3] == ' ' || rest[3] == '\t') {
		y.add(rest[:3], hlKeyword)
		y.value(rest[3:])
		return y.spans
	}
	for rest == "-" || strings.HasPrefix(rest, "- ") {
		y.add("-", hlOp)
		t := strings.TrimLeft(rest[1:], " ")
		y.add(rest[1:len(rest)-len(t)], hlPlain)
		rest = t
	}
	switch {
	case rest == "":
	case rest[0] == '#':
		y.add(rest, hlComment)
	default:
		if k := yamlKeyEnd(rest); k > 0 {
			y.add(rest[:k], hlKey)
			y.add(":", hlOp)
			rest = rest[k+1:]
		}
		y.value(rest)
	}
	return y.spans
}

// yamlKeyEnd is the index of the colon that ends the mapping key at the start of s, or -1 when s doesn't start with a
// key. A quoted key may hold anything; a plain one ends at a colon followed by a space or the end, and a # after a space
// is a comment before any colon.
func yamlKeyEnd(s string) int {
	switch s[0] {
	case '"', '\'':
		j := yamlQuoteEnd(s)
		if j < 0 {
			return -1
		}
		t := j + 1
		for t < len(s) && s[t] == ' ' {
			t++
		}
		if t < len(s) && s[t] == ':' && (t+1 == len(s) || s[t+1] == ' ') {
			return t
		}
		return -1
	case '[', '{', '&', '*', '!', '|', '>', '%', '@', '?', '#', ',', ']', '}':
		return -1
	}
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t'):
			return -1
		case s[i] == ':' && (i+1 == len(s) || s[i+1] == ' '):
			return i
		}
	}
	return -1
}

// yamlQuoteEnd is the index of the quote that closes the quoted scalar s starts with, or -1.
func yamlQuoteEnd(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

// quoted adds the part of s from i that is inside the open quote, up to and with the closing one, and returns where it
// stopped.
func (y *yamlLexer) quoted(s string, i int) int {
	start := i
	for i < len(s) {
		switch c := s[i]; {
		case y.quote == '"' && c == '\\':
			i = min(i+2, len(s))
		case y.quote == '\'' && c == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i += 2
		case c == y.quote:
			y.add(s[start:i+1], hlString)
			y.quote = 0
			return i + 1
		default:
			i++
		}
	}
	y.add(s[start:], hlString)
	return len(s)
}

// value adds s, what follows a key, a dash or a document marker: scalars, flow collections, anchors, tags, a block
// scalar's header, a comment.
func (y *yamlLexer) value(s string) {
	for i := 0; i < len(s); {
		c := s[i]
		first := strings.TrimSpace(s[:i]) == "" // nothing but blanks before: the start of the value
		switch {
		case c == ' ' || c == '\t':
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			y.add(s[i:j], hlPlain)
			i = j
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			y.add(s[i:], hlComment)
			return
		case c == '"' || c == '\'':
			y.quote = c
			y.add(string(c), hlString)
			i = y.quoted(s, i+1)
		case (c == '|' || c == '>') && first && blockHeader(s[i:]) > 0:
			n := blockHeader(s[i:])
			y.add(s[i:i+n], hlOp)
			y.block = true
			i += n
		case c == '&' || c == '*' || c == '!':
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '\t' && !(y.flow > 0 && strings.IndexByte(",]}", s[j]) >= 0) {
				j++
			}
			y.add(s[i:j], hlKeyword)
			i = j
		case (c == '[' || c == '{') && (first || y.flow > 0):
			y.flow++
			y.add(string(c), hlOp)
			i++
		case y.flow > 0 && strings.IndexByte(",]}", c) >= 0:
			if c != ',' {
				y.flow--
			}
			y.add(string(c), hlOp)
			i++
		case y.flow > 0 && c == ':' && (i+1 == len(s) || s[i+1] == ' '):
			y.add(":", hlOp)
			i++
		default:
			j := i
			for j < len(s) {
				if s[j] == '#' && j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
					break
				}
				if y.flow > 0 && (strings.IndexByte(",]}", s[j]) >= 0 || s[j] == ':' && (j+1 == len(s) || s[j+1] == ' ')) {
					break
				}
				j++
			}
			tok := strings.TrimRight(s[i:j], " \t")
			if tok == "" {
				tok = s[i : i+1]
			}
			k := yamlScalar(tok)
			if y.flow > 0 && i+len(tok) < len(s) && s[i+len(tok)] == ':' {
				k = hlKey
			}
			y.add(tok, k)
			i += len(tok)
		}
	}
}

// blockHeader is the length of the header of a block scalar at the start of s ("|", ">-", "|2+"), or 0 when s isn't one:
// only blanks or a comment may follow it on the line.
func blockHeader(s string) int {
	j := 1
	for j < len(s) && strings.IndexByte("+-0123456789", s[j]) >= 0 {
		j++
	}
	t := strings.TrimLeft(s[j:], " \t")
	if t == "" {
		return j
	}
	if t[0] == '#' && t != s[j:] { // a comment needs a blank before it
		return j
	}
	return 0
}

// yamlScalar is the kind of the plain scalar tok: null and booleans like keywords, numbers, and anything else a string.
func yamlScalar(tok string) hlKind {
	switch tok {
	case "true", "false", "null", "~", "True", "False", "Null", "TRUE", "FALSE", "NULL":
		return hlKeyword
	}
	if strings.IndexByte("0123456789-+.", tok[0]) >= 0 {
		if _, err := strconv.ParseFloat(tok, 64); err == nil {
			return hlNumber
		}
		if _, err := strconv.ParseInt(tok, 0, 64); err == nil {
			return hlNumber
		}
	}
	return hlString
}
