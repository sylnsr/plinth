package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"plinth/internal/gitrepo"
)

// Finding is one thing Check found in .plinth/.
type Finding struct {
	File    string // the path inside .plinth/
	Line    int    // 1-based; 0 for the file as a whole
	Refused bool   // a rule: the image isn't built; else flagged, for the user to look at before publish
	Text    string
}

// String is the finding on one line; a control character (from a path or the Containerfile) shows as ?.
func (f Finding) String() string {
	kind := "flagged"
	if f.Refused {
		kind = "refused"
	}
	s := fmt.Sprintf("%s: %s: %s", kind, f.File, f.Text)
	if f.Line > 0 {
		s = fmt.Sprintf("%s: %s:%d: %s", kind, f.File, f.Line, f.Text)
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// Refused is the findings that stop a build.
func Refused(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Refused {
			out = append(out, f)
		}
	}
	return out
}

// Check checks the files of .plinth/: the tools decide. Refused: a Containerfile
// that is missing or has no FROM; a FROM, COPY --from or RUN --mount from= that names neither an earlier stage nor an
// image pinned by digest (or names it through a variable); an ADD of anything but local files (a URL, a git repo, a
// variable); a RUN --network= other than none or default, or --security= other than sandbox. And, in every text file
// of the context, since a script the Containerfile copies in runs in the build too: a download run as it arrives (curl
// ... | sh and the like), or one not checked against a checksum written out on the same line. Flagged, not refused: an
// ansible-galaxy call, the user's choice to make.
func Check(files []gitrepo.File) []Finding {
	var out []Finding
	found := false
	for _, f := range files {
		switch {
		case f.Path == Containerfile:
			found = true
			out = append(out, checkContainerfile(f.Data)...)
			var ins []instruction
			for _, in := range instructions(f.Data) {
				_, rest := splitFlags(in.args)
				ins = append(ins, instruction{line: in.line, cmd: in.cmd, args: rest})
			}
			out = append(out, downloads(f.Path, ins)...)
			out = append(out, galaxy(f.Path, ins)...)
		case !bytes.Contains(f.Data[:min(len(f.Data), 8192)], []byte{0}): // text, not a binary
			lines := logicalLines(f.Data)
			out = append(out, downloads(f.Path, lines)...)
			out = append(out, galaxy(f.Path, lines)...)
		}
	}
	if !found {
		out = append([]Finding{{File: Containerfile, Refused: true, Text: "missing"}}, out...)
	}
	return out
}

// instruction is one Containerfile instruction: continuation lines joined, comments left out.
type instruction struct {
	line int    // its first line, 1-based
	cmd  string // upper-case: FROM, COPY, ...
	args string
}

var (
	directiveRe = regexp.MustCompile(`^#\s*([A-Za-z]+)\s*=\s*(\S*)\s*$`)
	heredocRe   = regexp.MustCompile(`(^|[^<])<<[^<]`) // <<EOF, <<-EOF, <<"EOF", 3<<EOF; not <<<
)

// instructions parses a Containerfile the way podman build does, as far as Check needs: the escape parser directive
// (an agent could set a backtick to hide a continuation), continuation lines and comments. Heredocs aren't parsed:
// checkContainerfile refuses them, since a body Check misreads could hide an instruction or make up a stage name.
func instructions(cf []byte) []instruction {
	lines := strings.Split(strings.ReplaceAll(string(cf), "\r\n", "\n"), "\n")
	escape := `\`
	i := 0
	for ; i < len(lines); i++ { // parser directives come first
		m := directiveRe.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		if strings.EqualFold(m[1], "escape") && m[2] == "`" {
			escape = "`"
		}
	}
	var out []instruction
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		in := instruction{line: i + 1}
		var b strings.Builder
		for {
			t := strings.TrimRight(lines[i], " \t")
			if !strings.HasSuffix(t, escape) {
				b.WriteString(t)
				break
			}
			b.WriteString(strings.TrimSuffix(t, escape) + " ")
			for i++; i < len(lines); i++ { // comments and empty lines inside a continuation are dropped
				if s := strings.TrimSpace(lines[i]); s != "" && !strings.HasPrefix(s, "#") {
					break
				}
			}
			if i >= len(lines) {
				break
			}
		}
		cmd, args, _ := strings.Cut(strings.TrimSpace(b.String()), " ")
		in.cmd, in.args = strings.ToUpper(cmd), strings.TrimSpace(args)
		out = append(out, in)
	}
	return out
}

var (
	pinned  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	stageNo = regexp.MustCompile(`^[0-9]+$`)
)

// checkContainerfile checks every image a Containerfile names, and every ADD.
func checkContainerfile(cf []byte) []Finding {
	var out []Finding
	refuse := func(line int, format string, a ...any) {
		out = append(out, Finding{File: Containerfile, Line: line, Refused: true, Text: fmt.Sprintf(format, a...)})
	}
	stages := map[string]bool{}
	n := 0 // stages so far
	// source checks what --from= or from= names: an earlier stage, by name or number, or a pinned image.
	source := func(line int, what, s string) {
		switch {
		case strings.Contains(s, "$"):
			refuse(line, "%s=%s: a variable can't be checked", what, s)
		case stages[strings.ToLower(s)], stageNo.MatchString(s) && mustAtoi(s) < n, pinned.MatchString(s):
		default:
			refuse(line, "%s=%s names neither an earlier stage nor an image pinned by digest (name@sha256:<64 hex>)", what, s)
		}
	}
	for _, in := range instructions(cf) {
		flags, rest := splitFlags(in.args)
		if heredocRe.MatchString(in.args) {
			refuse(in.line, "%s with a heredoc (<<WORD): Plinth's check doesn't read heredocs; put the script in a file in .plinth/ and COPY it", in.cmd)
			continue
		}
		switch in.cmd {
		case "ONBUILD": // its instruction runs in a later build, FROM this image, where no check sees it (devops review of a6e9ad6)
			refuse(in.line, "ONBUILD: a runner image has no use for it, and Plinth's check doesn't read the instruction it holds")
		case "FROM":
			f := strings.Fields(rest)
			if len(f) == 0 {
				refuse(in.line, "FROM without an image")
				continue
			}
			switch img := f[0]; {
			case strings.Contains(img, "$"):
				refuse(in.line, "FROM %s: a variable can't be checked", img)
			case img == "scratch", stages[strings.ToLower(img)], pinned.MatchString(img):
			default:
				refuse(in.line, "FROM %s is not pinned by digest (name@sha256:<64 hex>)", img)
			}
			if len(f) >= 3 && strings.EqualFold(f[1], "as") {
				stages[strings.ToLower(f[2])] = true
			}
			n++
		case "COPY", "ADD":
			for _, fl := range flags {
				if v, ok := strings.CutPrefix(fl, "--from="); ok {
					source(in.line, "--from", v)
				}
				if in.cmd == "ADD" && strings.HasPrefix(fl, "--keep-git-dir") {
					refuse(in.line, "ADD --keep-git-dir: ADD takes local files only")
				}
			}
			if in.cmd == "ADD" {
				for _, s := range addSources(rest) {
					switch {
					case strings.Contains(s, "$"):
						refuse(in.line, "ADD %s: a variable can't be checked", s)
					case strings.Contains(s, "://") || strings.HasPrefix(s, "git@"):
						refuse(in.line, "ADD %s: ADD takes local files only (no URL, no git repo)", s)
					}
				}
			}
		case "RUN":
			for _, fl := range flags {
				name, v, _ := strings.Cut(fl, "=")
				v = strings.Trim(v, `"'`)
				switch strings.ToLower(name) {
				case "--mount":
					for _, opt := range strings.Split(v, ",") {
						if k, val, _ := strings.Cut(strings.Trim(opt, `"'`), "="); strings.EqualFold(k, "from") {
							source(in.line, "--mount from", strings.Trim(val, `"'`))
						}
					}
				case "--network": // host would give the step this machine's local services, which runs never reach
					if v != "none" && v != "default" {
						refuse(in.line, "RUN --network=%s: a build step gets none or default only", v)
					}
				case "--security":
					if v != "sandbox" {
						refuse(in.line, "RUN --security=%s: a build step runs sandboxed only", v)
					}
				}
			}
		}
	}
	if n == 0 {
		refuse(0, "no FROM")
	}
	return out
}

// Base is an image a Containerfile's FROM names, pinned by digest: neither scratch nor an earlier stage.
type Base struct {
	Line     int
	Ref      string
	Platform string // the FROM's --platform; "" without one, or for a variable, which podman build sets to this machine's
}

// Bases lists the images the Containerfile's FROM lines name, for image publish to look inside: their ONBUILD triggers
// run in the build, where Check doesn't see them. Check refuses whatever else a FROM names.
func Bases(cf []byte) []Base {
	var out []Base
	stages := map[string]bool{}
	for _, in := range instructions(cf) {
		if in.cmd != "FROM" {
			continue
		}
		flags, rest := splitFlags(in.args)
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if img := f[0]; !stages[strings.ToLower(img)] && pinned.MatchString(img) {
			b := Base{Line: in.line, Ref: img}
			for _, fl := range flags {
				if v, ok := strings.CutPrefix(fl, "--platform="); ok && !strings.Contains(v, "$") {
					b.Platform = v
				}
			}
			out = append(out, b)
		}
		if len(f) >= 3 && strings.EqualFold(f[1], "as") {
			stages[strings.ToLower(f[2])] = true
		}
	}
	return out
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1 << 30
	}
	return n
}

// splitFlags splits an instruction's leading --flags from the rest.
func splitFlags(args string) (flags []string, rest string) {
	rest = args
	for strings.HasPrefix(rest, "--") {
		fl, after, _ := strings.Cut(rest, " ")
		flags = append(flags, fl)
		rest = strings.TrimSpace(after)
	}
	return flags, rest
}

// addSources is an ADD's sources: all but the last argument, in the shell form or the JSON form.
func addSources(rest string) []string {
	var args []string
	if !strings.HasPrefix(rest, "[") || json.Unmarshal([]byte(rest), &args) != nil {
		args = strings.Fields(rest)
	}
	if len(args) < 2 {
		return args
	}
	return args[:len(args)-1]
}

// galaxy flags each line that names ansible-galaxy: its install paths have had High CVEs, one unfixed in Debian when
// decided (CVE-2026-89091, collection tarballs). Plinth's way is collections vendored in .plinth/ as
// extracted trees and copied in, so no ansible-galaxy code runs; whether the image uses it all the same is the user's
// call, so the finding only asks them to look.
func galaxy(path string, lines []instruction) []Finding {
	var out []Finding
	for _, l := range lines {
		if galaxyRe.MatchString(l.args) {
			out = append(out, Finding{File: path, Line: l.line, Text: "runs ansible-galaxy, whose install paths have had " +
				"High CVEs (CVE-2026-89091 unfixed in Debian): vendor collections in .plinth/collections/ as extracted " +
				"trees and COPY them instead (docs/runner-image.md)"})
		}
	}
	return out
}

// galaxyRe is ansible-galaxy named on a line.
var galaxyRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])ansible-galaxy(?:$|[^A-Za-z0-9_.-])`)

// fetchRunRe is a download piped or substituted into a shell or an interpreter.
var fetchRunRe = regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b[^|;&\n]*\|\s*(sudo\s+(-\S+\s+)*)?(env\s+(\S+=\S*\s+)*)?` +
	`(ba|da|k|z|fi)?sh\b|\b(curl|wget|fetch)\b[^|;&\n]*\|\s*(sudo\s+(-\S+\s+)*)?(python[0-9.]*|perl|ruby|node|php)\b|` +
	`\b(ba|da|k|z)?sh\s+(-c\s+)?["']?(\$\(|<\(|` + "`" + `)\s*(curl|wget|fetch)\b|\bsource\s+<\(\s*(curl|wget)\b`)

var (
	// downloadRe is curl or wget as a command: first on the line, after a separator, after sudo, env and the like, or
	// first in an exec form or a -c string; not as an argument (apt-get install curl).
	downloadRe = regexp.MustCompile(`(?:^|[;&|({` + "`" + `]|\$\(|\[\s*"|-c\s+["']?|` +
		`\b(?:sudo|env|then|do|else|exec|time|nohup|command|xargs)\s+(?:-\S+\s+|\S+=\S*\s+)*)\s*(?:/\S*/)?(curl|wget)(?:$|[\s"';&|)])`)
	// checksumRe is a checksum check, sha256 or stronger; digestRe a checksum written out (sha256: 64 hex, sha512: 128).
	checksumRe = regexp.MustCompile(`\b(?:sha256sum|sha384sum|sha512sum|b2sum|shasum\s+(?:-\S+\s+)*-a\s*(?:256|384|512))\b` +
		`[^;&|]*\s(?:-[A-Za-z]*c[A-Za-z]*|--check)\b`)
	digestRe = regexp.MustCompile(`(?:^|[^0-9A-Fa-f])[0-9A-Fa-f]{64,128}(?:$|[^0-9A-Fa-f])`)
	urlRe    = regexp.MustCompile(`\S*://\S*`)
)

// logicalLines is a file's lines, continuation lines joined.
func logicalLines(data []byte) []instruction {
	var out []instruction
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		first, joined := i+1, lines[i]
		for strings.HasSuffix(strings.TrimRight(joined, " \t"), `\`) && i+1 < len(lines) {
			i++
			joined = strings.TrimSuffix(strings.TrimRight(joined, " \t"), `\`) + " " + lines[i]
		}
		out = append(out, instruction{line: first, args: strings.TrimSpace(joined)})
	}
	return out
}

// downloads refuses each line (a Containerfile instruction, or a line of another file with its continuation lines)
// that runs what it downloads as it arrives, or that downloads with curl or wget and doesn't check what it got against
// a checksum written out on that same line (decision 2): the tool can't tell whether a file is run
// later, and whatever it is, the image holds what the URL served at build time. A checksum inside a URL doesn't count.
func downloads(path string, lines []instruction) []Finding {
	const how = "vendor the file into .plinth/ (preferred), or pin it by checksum on the same line: " +
		"curl -fsSLo f <url> && echo '<sha256>  f' | sha256sum -c && <use f>"
	var out []Finding
	for _, l := range lines {
		switch {
		case fetchRunRe.MatchString(l.args):
			out = append(out, Finding{File: path, Line: l.line, Refused: true,
				Text: "runs what it downloads as it arrives (curl ... | sh and the like): " + how})
		case downloadRe.MatchString(l.args) &&
			!(checksumRe.MatchString(l.args) && digestRe.MatchString(urlRe.ReplaceAllString(l.args, " "))):
			out = append(out, Finding{File: path, Line: l.line, Refused: true,
				Text: "downloads without checking it against a checksum written out on the same line: " + how})
		}
	}
	return out
}
