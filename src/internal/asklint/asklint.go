// Package asklint is ask lint: the dry-run check of an ask script before anyone runs it. Plain AAA checks a script in
// its run directory; with Plinth set it is the check every submission gets (ask.lint in decide), before the reviews
// and the user. Every finding is one line. Spec: plinthctl.README.md, "No backend" (ask lint) and "Runs".
package asklint

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"plinth/internal/term"
)

// MaxLines is the most lines an ask script may have.
const MaxLines = 500

// LibLine is a plain AAA script's one fixed line: it sources the AAA library, which plinthctl prints, and stops
// the script when that didn't load.
const LibLine = `source <(plinthctl ask lib); [[ "${AAA_LOADED:-}" == 1 ]] || { echo "$RUN_NAME: the AAA library didn't load (plinthctl ask lib) - nothing was run" >&2; exit 2; }`

// PlinthLibLine is the one fixed line of a script the controller runs: it sources the AAA library the controller
// provides for the run, read-only, at $AAA_LIB.
const PlinthLibLine = `source "$AAA_LIB"; [[ "${AAA_LOADED:-}" == 1 ]] || { echo "$RUN_NAME: the AAA library didn't load (\$AAA_LIB) - nothing was run" >&2; exit 2; }`

// Options say what to check besides the basics.
type Options struct {
	// RunName, when set, is the run directory's name: RUN_NAME must equal it (plain AAA). A submission has none yet.
	RunName string
	// Plinth is --plinth: the controller's library line (PlinthLibLine) instead of plain AAA's, the run modes, no relative
	// path (no .. path component) and nothing naming Work. A script may name Plinth itself: whether it reaches into
	// Plinth is for the security and DLP reviews.
	Plinth bool
	// Work is Plinth's work directory, which a script must not name (with Plinth).
	Work string
	// ForgejoURL is forgejo-url: when set, an ISSUE= line must name an issue on that Forgejo.
	ForgejoURL string
}

var (
	headerRe      = regexp.MustCompile(`^# AAA script: (.*)$`)
	runNameRe     = regexp.MustCompile(`(?m)^RUN_NAME="([^"]*)"`)
	agentNameRe   = regexp.MustCompile(`(?m)^AGENT_NAME="([^"]*)"`)
	placeholderRe = regexp.MustCompile(`^<[^>]*>`)
	// anyPlaceholderRe is a template placeholder such as <section> or <exact list>.
	anyPlaceholderRe = regexp.MustCompile(`<[a-z][^<>]*>`)
	// dotdot is a .. path component: ../x, x/.., a lone .. (cd ..).
	dotdotRe = regexp.MustCompile(`(^|[\s/"'=:(])\.\.($|[\s/"');|&])`)

	// No ssh in a script that isn't privileged. commentRe is a comment, from a # that starts a
	// word. sshWordRe is an ssh program (or the plain-AAA helper ssh_q) as a word of its own, so also ssh:// and
	// /usr/bin/ssh, but not ~/.ssh or ssh-keygen; sshVarRe the variables and options that make git or rsync use ssh;
	// scpURLRe a user@host:path remote (git@forgejo.example.org:owner/repo.git); rsyncRe rsync, which takes host:path
	// over ssh unless told otherwise.
	commentRe = regexp.MustCompile(`(^|\s)#.*$`)
	sshWordRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])(ssh|scp|sftp|slogin|ssh_q)(?:$|[^A-Za-z0-9_.-])`)
	sshVarRe  = regexp.MustCompile(`\b(GIT_SSH(?:_COMMAND|_VARIANT)?|RSYNC_RSH|core\.sshCommand)\b|--rsh\b`)
	scpURLRe  = regexp.MustCompile(`(?:^|[\s"'=])([A-Za-z0-9._-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+:)`)
	rsyncRe   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])rsync(?:$|[^A-Za-z0-9_.-])`)
	// hostPathRe is an rsync argument naming a remote host:path (rsync://host and host::module are its daemon, not ssh).
	hostPathRe = regexp.MustCompile(`^[A-Za-z0-9._@${}-]+:(?:$|[^:/]|/[^/])`)

	// A plain AAA script holds only the agent's lines: funcRe is a function definition, ownRe what only the AAA
	// library writes or uses: the answer's files, its own names, a trap.
	funcRe = regexp.MustCompile(`^\s*(function\s+[A-Za-z_][A-Za-z0-9_:.-]*|[A-Za-z_][A-Za-z0-9_:.-]*\s*\(\s*\))`)
	ownRe  = regexp.MustCompile(`running\.yml|answer[A-Za-z.-]*\.yml|\b_?AAA_[A-Z_]+|\b_aaa_|\btrap\b`)
)

// Check returns one line per finding; none means the script passed.
func Check(script []byte, o Options) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if !utf8.Valid(script) {
		add("not UTF-8")
		return out
	}
	text := string(script)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	// Printable text only: a control, bidi or invisible character could hide or reorder lines while the user
	// reads the script, or drive the terminal it is shown in.
	for i, l := range lines {
		for _, r := range l {
			if term.Unprintable(r) {
				add("line %d holds %s, a control or invisible character: a script is printable text, tabs and newlines only", i+1, term.Escape(r))
				break
			}
		}
	}
	if len(lines) > MaxLines {
		add("%d lines, at most %d", len(lines), MaxLines)
	}
	if len(lines) == 0 || lines[0] != "#!/bin/bash" {
		add("line 1 is not #!/bin/bash")
	}
	if len(lines) < 2 {
		add("no header: line 2 must be # AAA script: <what this round is for>")
	} else if m := headerRe.FindStringSubmatch(lines[1]); m == nil || strings.TrimSpace(m[1]) == "" || placeholderRe.MatchString(m[1]) {
		add("no header: line 2 must be # AAA script: <what this round is for>, filled in")
	}
	mode := false
	for i := 1; i < min(len(lines), 8) && strings.HasPrefix(lines[i], "#"); i++ {
		if i > 1 && anyPlaceholderRe.MatchString(lines[i]) {
			add("header line %d still holds a <...> placeholder", i+1)
		}
		if strings.Contains(lines[i], "READ-ONLY") || strings.Contains(lines[i], "CHANGES:") {
			mode = true
		}
	}
	if !mode {
		add("the header doesn't say READ-ONLY or CHANGES: <exact list>")
	}
	if ms := runNameRe.FindAllStringSubmatch(text, -1); len(ms) != 1 {
		add("%d RUN_NAME=\"...\" lines, want exactly 1", len(ms))
	} else if o.RunName != "" && ms[0][1] != o.RunName {
		add("RUN_NAME %q does not match the run directory %s", ms[0][1], o.RunName)
	}
	if ms := agentNameRe.FindAllStringSubmatch(text, -1); len(ms) != 1 {
		add("%d AGENT_NAME=\"...\" lines, want exactly 1", len(ms))
	}
	interactive := flag(text, "INTERACTIVE", true, add)
	// The run modes: with Plinth every script says its mode; plain AAA has none, but a line there must be valid.
	flag(text, "NETWORK", o.Plinth, add)
	privileged := flag(text, "PRIVILEGED", o.Plinth, add)
	if o.Plinth && interactive && !privileged {
		add("INTERACTIVE=1 needs PRIVILEGED=1: an interactive script runs in the user's terminal, outside its container")
	}
	if o.Plinth && !privileged {
		for i, l := range lines {
			if what := usesSSH(l); what != "" {
				add("line %d uses ssh (%s): a script that isn't privileged gets no ssh; remote work belongs to Ansible, and git "+
					"goes over https with a token from the env file", i+1, what)
			}
		}
	}
	if o.Plinth {
		out = append(out, checkLib(lines, PlinthLibLine, "a script the controller runs", "RUN_NAME=", "AGENT_NAME=", "INTERACTIVE=", "NETWORK=", "PRIVILEGED=")...)
	} else {
		out = append(out, checkLib(lines, LibLine, "a plain AAA script", "RUN_NAME=", "AGENT_NAME=", "INTERACTIVE=")...)
	}
	if o.Plinth {
		for i, l := range lines {
			if o.Work != "" && strings.Contains(l, o.Work) {
				add("line %d names Plinth's work directory", i+1)
			}
			if dotdotRe.MatchString(l) {
				add("line %d has a relative path (a .. component)", i+1)
			}
		}
	}
	out = append(out, checkIssue(script, o.ForgejoURL)...)
	if msg := bashN(script); msg != "" {
		add("bash -n: %s", msg)
	}
	return out
}

// checkLib checks what a script of the AAA library must be, what (a plain AAA script, or one the controller
// runs): the agent's lines and the library line lib, nothing else. The library line comes once, after the variables
// vars, which the library reads; no function is defined (the helpers are the library's) and nothing names the
// answer's files, the library's own names or a trap (only the library writes the answer). Like the ssh check it reads
// words, comments left out, not shell syntax.
func checkLib(lines []string, lib, what string, vars ...string) []string {
	var out []string
	at := -1
	for i, l := range lines {
		if strings.TrimRight(l, " \t") != lib {
			continue
		}
		if at >= 0 {
			out = append(out, fmt.Sprintf("line %d: the AAA library line again, want it once", i+1))
		}
		at = i
	}
	if at < 0 {
		out = append(out, "no AAA library line: "+what+" sources the library with exactly this line (as its template has it): "+lib)
	}
	for _, v := range vars {
		for i, l := range lines {
			if strings.HasPrefix(l, v) && at >= 0 && i > at {
				out = append(out, fmt.Sprintf("line %d: %s comes after the AAA library line, which reads it", i+1, strings.TrimSuffix(v, "=")))
			}
		}
	}
	for i, l := range lines {
		if i == at {
			continue
		}
		l = commentRe.ReplaceAllString(l, "")
		if m := funcRe.FindString(l); m != "" {
			out = append(out, fmt.Sprintf("line %d defines a function (%s): the helpers are the AAA library's", i+1, strings.TrimSpace(m)))
		}
		if m := ownRe.FindString(l); m != "" {
			out = append(out, fmt.Sprintf("line %d names %s: only the AAA library writes the answer", i+1, m))
		}
	}
	return out
}

// usesSSH names what in line l (its comment left out) runs ssh, or makes git or rsync run it; "" for nothing. It is a
// check of words, not a shell parser: a word in a string counts too, and the user can always run such a script as
// privileged.
func usesSSH(l string) string {
	l = commentRe.ReplaceAllString(l, "")
	if m := sshWordRe.FindStringSubmatch(l); m != nil {
		return m[1]
	}
	if m := sshVarRe.FindString(l); m != "" {
		return m
	}
	if m := scpURLRe.FindStringSubmatch(l); m != nil {
		return "a remote " + m[1] + "..."
	}
	if rsyncRe.MatchString(l) {
		for _, f := range strings.Fields(l) {
			if f = strings.Trim(f, `"'`); !strings.HasPrefix(f, "-") && hostPathRe.MatchString(f) {
				return "rsync to " + f
			}
		}
	}
	return ""
}

// flag checks the script's <name>= lines: exactly one when required, else at most one, saying 0 or 1. It reports
// whether the line says 1.
func flag(text, name string, required bool, add func(string, ...any)) bool {
	ms := regexp.MustCompile(`(?m)^`+name+`=(\S*)`).FindAllStringSubmatch(text, -1)
	switch {
	case len(ms) > 1 || (required && len(ms) == 0):
		add("%d %s= lines, want exactly 1", len(ms), name)
	case len(ms) == 0:
	case ms[0][1] != "0" && ms[0][1] != "1":
		add("%s=%s, want 0 or 1", name, ms[0][1])
	default:
		return ms[0][1] == "1"
	}
	return false
}

// bashN parses the script with bash -n (nothing runs) and returns bash's first complaint, or "".
func bashN(script []byte) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-n")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimPrefix(msg, "bash: ")
	}
	return ""
}
