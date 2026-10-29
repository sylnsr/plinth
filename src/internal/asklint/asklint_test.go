package asklint

import (
	"os/exec"
	"strings"
	"testing"

	"plinth/internal/workdir"
)

// filled is the namespace template with its header filled in, the way an agent submits it.
func filled(t *testing.T) string {
	t.Helper()
	return fill(t, workdir.AskTemplate)
}

// filledAAA is plain AAA's template, filled in the same way.
func filledAAA(t *testing.T) string {
	t.Helper()
	return fill(t, workdir.AAATemplate)
}

func fill(t *testing.T, tpl []byte) string {
	t.Helper()
	s := strings.Replace(string(tpl), "# AAA script: <one line: what this round is for>", "# AAA script: check the disks", 1)
	s = strings.Replace(s, "# READ-ONLY diagnostics - nothing is started, stopped or changed.   (or: CHANGES: <exact list>)", "# READ-ONLY diagnostics - nothing is started, stopped or changed.", 1)
	s = strings.Replace(s, "#   1 <section>  2 <section>  3 <section>", "#   1 disks  2 smart", 1)
	if s == string(tpl) {
		t.Fatal("the template's header line changed; update this test")
	}
	return s
}

func needBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
}

func has(findings []string, sub string) bool {
	for _, f := range findings {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}

func TestTemplatePasses(t *testing.T) {
	needBash(t)
	if f := Check([]byte(filled(t)), Options{Plinth: true, Work: "/home/u/.work"}); len(f) != 0 {
		t.Errorf("the filled-in template fails ask lint --plinth:\n%s", strings.Join(f, "\n"))
	}
	// As it is, its header is placeholders: what the round does, READ-ONLY or CHANGES (the template offers both), and
	// the sections. An agent must fill in all of it.
	f := Check(workdir.AskTemplate, Options{Plinth: true})
	if !has(f, "no header") || !has(f, "header line 3 still holds a <...> placeholder") || !has(f, "header line 4 still holds") || len(f) != 3 {
		t.Errorf("the template as it is: %v", f)
	}
}

func TestFindings(t *testing.T) {
	needBash(t)
	base := filled(t)
	for _, c := range []struct {
		name   string
		script string
		o      Options
		want   string
	}{
		{"not utf-8", base + "echo \xff\n", Options{}, "not UTF-8"},
		// Printable text only: what could drive the terminal or hide and reorder lines while the user reads it.
		{"an escape sequence", base + "echo \x1b]0;title\x07\n", Options{}, `holds \x1b, a control or invisible character`},
		{"a carriage return", base + "echo hidden\rshown\n", Options{}, `holds \x0d`},
		{"a bidi override", base + "echo \u202eevil\n", Options{}, `holds \u202e`},
		{"a zero-width space", base + "echo a\u200bb\n", Options{}, `holds \u200b`},
		{"a C1 control", base + "echo \u009b31m\n", Options{}, `holds \u009b`},
		{"too long", base + strings.Repeat("echo\n", MaxLines), Options{}, "lines, at most 500"},
		{"no shebang", strings.Replace(base, "#!/bin/bash", "#!/bin/sh", 1), Options{}, "line 1 is not #!/bin/bash"},
		{"no mode", strings.Replace(base, "READ-ONLY diagnostics", "diagnostics", 1), Options{}, "doesn't say READ-ONLY or CHANGES"},
		{"mode left a placeholder", strings.Replace(base, "# READ-ONLY diagnostics - nothing is started, stopped or changed.", "# CHANGES: <exact list>", 1), Options{}, "header line 3 still holds"},
		{"two RUN_NAMEs", base + "RUN_NAME=\"x\"\n", Options{}, "2 RUN_NAME=\"...\" lines"},
		{"no AGENT_NAME", strings.Replace(base, "AGENT_NAME=\"\"", "AGENT=\"\"", 1), Options{}, "0 AGENT_NAME=\"...\" lines"},
		{"INTERACTIVE=yes", strings.Replace(base, "INTERACTIVE=0", "INTERACTIVE=yes", 1), Options{}, "INTERACTIVE=yes, want 0 or 1"},
		{"no NETWORK", strings.Replace(base, "NETWORK=0", "NET=0", 1), Options{Plinth: true}, "0 NETWORK= lines, want exactly 1"},
		{"two PRIVILEGED", base + "PRIVILEGED=1\n", Options{}, "2 PRIVILEGED= lines"},
		{"PRIVILEGED=2", strings.Replace(base, "PRIVILEGED=0", "PRIVILEGED=2", 1), Options{}, "PRIVILEGED=2, want 0 or 1"},
		{"interactive, not privileged", strings.Replace(base, "INTERACTIVE=0", "INTERACTIVE=1", 1), Options{Plinth: true}, "INTERACTIVE=1 needs PRIVILEGED=1"},
		{"wrong RUN_NAME", base, Options{RunName: "keen-tapir"}, "does not match the run directory keen-tapir"},
		{"syntax", base + "if then\n", Options{}, "bash -n:"},
		{"work dir", base + "ls /home/u/.work\n", Options{Plinth: true, Work: "/home/u/.work"}, "names Plinth's work directory"},
		{"cd ..", base + "cd ..\n", Options{Plinth: true}, "relative path"},
		{"../", base + "cat \"$RUN_DIR/../other/answer.md\"\n", Options{Plinth: true}, "relative path"},
	} {
		if f := Check([]byte(c.script), c.o); !has(f, c.want) {
			t.Errorf("%s: findings %v, want %q", c.name, f, c.want)
		}
	}
	// Without --plinth those are fine; {1..3} is not a path.
	if f := Check([]byte(filledAAA(t)+"cd ..\necho {1..3} plinth\n"), Options{}); len(f) != 0 {
		t.Errorf("plain lint: %v", f)
	}
	if f := Check([]byte(base+"for i in {1..3}; do echo \"wait...\"; done\n"), Options{Plinth: true}); len(f) != 0 {
		t.Errorf("brace ranges and ellipses: %v", f)
	}
	// Plinth itself may be named, as the repo a script works on: the reviews judge that.
	if f := Check([]byte(base+"git -C /home/u/git/plinth log -3 2>&1 | block   # the plinth repo\necho \"$PLINTH_TOKEN\"\n"), Options{Plinth: true, Work: "/home/u/.plinth"}); len(f) != 0 {
		t.Errorf("naming plinth: %v", f)
	}
}

// TestModes: plain AAA scripts have no mode lines; with Plinth, an interactive script is privileged.
func TestModes(t *testing.T) {
	needBash(t)
	base := filled(t)
	plain := filledAAA(t) // it has no mode lines
	if f := Check([]byte(plain), Options{}); len(f) != 0 {
		t.Errorf("plain AAA without mode lines: %v", f)
	}
	both := strings.Replace(strings.Replace(base, "INTERACTIVE=0", "INTERACTIVE=1", 1), "PRIVILEGED=0", "PRIVILEGED=1", 1)
	if f := Check([]byte(both), Options{Plinth: true}); len(f) != 0 {
		t.Errorf("interactive and privileged: %v", f)
	}
}

// TestNoSSH: with Plinth, a script that isn't privileged gets no ssh, so ask lint refuses ssh and what makes git or
// rsync use it; git goes over https. A privileged script and plain AAA may use it.
func TestNoSSH(t *testing.T) {
	needBash(t)
	base := filled(t)
	for _, line := range []string{
		"ssh root@host.example.org uptime",
		"scp ./out.txt host.example.org:/tmp/",
		"sftp host.example.org",
		"ssh_q root host.example.org \"uptime\"",
		"up=\"$(ssh host.example.org true)\"",
		"/usr/bin/ssh host.example.org",
		"git clone ssh://git@forgejo.example.org:2222/owner/repo.git",
		"git clone git@forgejo.example.org:owner/repo.git",
		"export GIT_SSH=/bin/true",
		"git -c core.sshCommand=true fetch",
		"rsync -a -e 'ssh -p 2222' ./a/ host.example.org:/b/",
		"rsync -a ./out/ backup.example.org:/srv/out/",
		"rsync -a ./out/ \"deploy@$HOST:/srv/out/\"",
		"RSYNC_RSH=true rsync -a ./a/ ./b/",
	} {
		if f := Check([]byte(base+line+"\n"), Options{Plinth: true}); !has(f, "uses ssh") {
			t.Errorf("%q: findings %v, want uses ssh", line, f)
		}
	}
	for _, line := range []string{
		"git clone https://forgejo.example.org/owner/repo.git",
		"curl -fsS \"https://user:$TOKEN@api.example.org:8443/v1\"",
		"ls ~/.ssh 2> /dev/null || true",
		"ssh-keygen -l -f key.pub",
		"rsync -a ./a/ ./b/",
		"rsync -a rsync://mirror.example.org/pub/ ./pub/",
		"echo done   # no ssh here",
	} {
		if f := Check([]byte(base+line+"\n"), Options{Plinth: true}); len(f) != 0 {
			t.Errorf("%q: findings %v, want none", line, f)
		}
	}
	privileged := strings.Replace(base, "PRIVILEGED=0", "PRIVILEGED=1", 1) + "ssh root@host.example.org uptime\n"
	if f := Check([]byte(privileged), Options{Plinth: true}); len(f) != 0 {
		t.Errorf("a privileged script: %v", f)
	}
	if f := Check([]byte(filledAAA(t)+"ssh_q root host.example.org uptime\n"), Options{}); len(f) != 0 {
		t.Errorf("plain AAA: %v", f)
	}
}

// TestPlain: a plain AAA script is the agent's lines and the library line, once, after the lines the library reads; no
// function, nothing that writes the answer or touches the library's own names.
func TestPlain(t *testing.T) {
	needBash(t)
	base := filledAAA(t)
	if f := Check([]byte(base), Options{}); len(f) != 0 {
		t.Errorf("the filled-in template: %v", f)
	}
	if f := Check(workdir.AAATemplate, Options{}); !has(f, "no header") || len(f) != 3 {
		t.Errorf("the template as it is: %v", f)
	}
	for _, c := range []struct{ name, script, want string }{
		{"no library line", strings.Replace(base, LibLine, "source ./mine.bash", 1), "no AAA library line"},
		{"twice", base + LibLine + "\n", "the AAA library line again"},
		{"RUN_NAME after it", strings.Replace(strings.Replace(base, "INTERACTIVE=0", "", 1), LibLine, LibLine+"\nINTERACTIVE=0", 1), "INTERACTIVE comes after"},
		{"a function", base + "say() { echo \"$*\"; }\n", "defines a function (say()"},
		{"function keyword", base + "function helper {\n  :\n}\n", "defines a function (function helper"},
		{"its own trap", base + "trap 'echo bye' EXIT\n", "names trap"},
		{"writes the answer", base + "echo '- x' >> running.yml\n", "names running.yml"},
		{"another answer", base + "cp /tmp/x answer.new.yml\n", "names answer.new.yml"},
		{"the library's names", base + "_AAA_FAILURE=\"\"\n", "names _AAA_FAILURE"},
		{"its functions", base + "echo x | _aaa_text -indent 6\n", "names _aaa_"},
	} {
		if f := Check([]byte(c.script), Options{}); !has(f, c.want) {
			t.Errorf("%s: findings %v, want %q", c.name, f, c.want)
		}
	}
	if f := Check([]byte(base+"echo hi   # running.yml, trap, f() in a comment\nsay \"$(date)\" # ok\n( cd /tmp && ls )\n"), Options{}); len(f) != 0 {
		t.Errorf("comments and subshells: %v", f)
	}
}

// TestPlinthLib: a script the controller runs holds the agent's lines and the controller's library line (source
// "$AAA_LIB"), once, after the lines the library reads, the run modes among them.
func TestPlinthLib(t *testing.T) {
	needBash(t)
	base := filled(t)
	o := Options{Plinth: true}
	for _, c := range []struct{ name, script, want string }{
		{"no library line", strings.Replace(base, PlinthLibLine, "source /aaa/lib.bash", 1), "no AAA library line"},
		{"plain AAA's line", strings.Replace(base, PlinthLibLine, LibLine, 1), "no AAA library line"},
		{"twice", base + PlinthLibLine + "\n", "the AAA library line again"},
		{"NETWORK after it", strings.Replace(strings.Replace(base, "NETWORK=0", "", 1), PlinthLibLine, PlinthLibLine+"\nNETWORK=0", 1), "NETWORK comes after"},
		{"a function", base + "f() { :; }\n", "defines a function (f()"},
		{"writes the answer", base + "echo x >> running.yml\n", "names running.yml"},
		{"its own variables", base + "echo \"$AAA_LIB\"\n", "names AAA_LIB"},
	} {
		if f := Check([]byte(c.script), o); !has(f, c.want) {
			t.Errorf("%s: findings %v, want %q", c.name, f, c.want)
		}
	}
	// Plain AAA's check wants plain AAA's line.
	if f := Check([]byte(base), Options{}); !has(f, "no AAA library line") {
		t.Errorf("the controller's template as plain AAA: %v", f)
	}
}
