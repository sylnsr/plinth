package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/workdir"
)

func TestEnableAgent(t *testing.T) {
	work := t.TempDir()
	write(t, work, filled)
	g, err := LoadGlobal(work, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "michael", "devops")
	file := filepath.Join(dir, FileName)
	read := func() string {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	enabled := func() map[string]string {
		n, _, err := LoadNamespace(work, ns, g, baseEnv)
		if err != nil {
			t.Fatal(err)
		}
		return n.Enabled
	}

	// The template (comments only): its text stays as it is, the block is appended.
	tpl := string(workdir.NamespaceTemplate)
	write(t, dir, tpl)
	if e := enabled(); len(e) != 0 {
		t.Fatalf("the template enables %v", e)
	}
	if err := EnableAgent(work, ns, g, baseEnv, "main", "/home/u/git/devops"); err != nil {
		t.Fatal(err)
	}
	if got := read(); !strings.HasPrefix(got, tpl) {
		t.Errorf("the template's text changed:\n%s", got)
	}
	if e := enabled(); len(e) != 1 || e["main"] != "/home/u/git/devops" {
		t.Errorf("enabled = %v", e)
	}

	// A file with a document: edited by key, every other key and every comment kept.
	body := `# my repo's settings
reviewers:
  automatic: [security] # only this one here
plinth:
  # messages expire sooner here
  messages:
    expiration-minutes: 30
  agents:
    enabled:
      main: /home/u/git/devops # since October
`
	write(t, dir, body)
	if err := EnableAgent(work, ns, g, baseEnv, "al", "/home/u/git/devops-al"); err != nil {
		t.Fatal(err)
	}
	got := read()
	for _, want := range []string{"# my repo's settings", "# only this one here", "# messages expire sooner here", "# since October",
		"expiration-minutes: 30", "main: /home/u/git/devops", "al: /home/u/git/devops-al"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	n, _, err := LoadNamespace(work, ns, g, baseEnv)
	if err != nil || len(n.Automatic) != 1 || n.ExpirationMinutes != 30 || len(n.Enabled) != 2 {
		t.Errorf("after enable: %+v, %v", n, err)
	}
	// Enabling again for the same repo changes nothing; for another repo it replaces the entry.
	before := read()
	if err := EnableAgent(work, ns, g, baseEnv, "al", "/home/u/git/devops-al"); err != nil || read() != before {
		t.Errorf("enable again: %v, file changed: %v", err, read() != before)
	}
	if err := EnableAgent(work, ns, g, baseEnv, "al", "/srv/devops"); err != nil || enabled()["al"] != "/srv/devops" {
		t.Errorf("enable for another repo: %v, %v", err, enabled())
	}

	// Disable removes the entry, and only it.
	if ok, err := DisableAgent(work, ns, g, baseEnv, "al"); !ok || err != nil {
		t.Errorf("disable al: %v, %v", ok, err)
	}
	if e := enabled(); len(e) != 1 || e["main"] == "" {
		t.Errorf("after disable: %v", e)
	}
	if ok, err := DisableAgent(work, ns, g, baseEnv, "al"); ok || err != nil {
		t.Errorf("disable al twice: %v, %v", ok, err)
	}

	// A plinth: key without agents, and an enabled: with no value.
	write(t, dir, "plinth:\n  messages:\n    expiration-minutes: 30\n")
	if err := EnableAgent(work, ns, g, baseEnv, "main", "/r"); err != nil || enabled()["main"] != "/r" {
		t.Errorf("into plinth: %v, %v\n%s", err, enabled(), read())
	}
	write(t, dir, "plinth:\n  agents:\n    enabled: # none yet\n")
	if err := EnableAgent(work, ns, g, baseEnv, "main", "/r"); err != nil || enabled()["main"] != "/r" || !strings.Contains(read(), "# none yet") {
		t.Errorf("into enabled: with no value: %v, %v\n%s", err, enabled(), read())
	}
	// No namespace config.yml at all: written with the block alone.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := EnableAgent(work, ns, g, baseEnv, "main", "/r"); err != nil || enabled()["main"] != "/r" {
		t.Errorf("without a config.yml: %v, %v", err, enabled())
	}
}

func TestEnableAgentRefuses(t *testing.T) {
	work := t.TempDir()
	write(t, work, filled)
	g, err := LoadGlobal(work, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "michael", "devops")
	for _, c := range []struct{ agent, repo string }{{"user", "/r"}, {"Main", "/r"}, {"main", "relative"}, {"main", "/a/../b"}} {
		if err := EnableAgent(work, ns, g, baseEnv, c.agent, c.repo); err == nil {
			t.Errorf("EnableAgent(%q, %q) passed", c.agent, c.repo)
		}
	}
	// A config that wouldn't load after the edit is put back as it was.
	bad := "plinth:\n  agents:\n    enabled:\n      main: /r\n      Bad: /x\n"
	write(t, dir, bad)
	if err := EnableAgent(work, ns, g, baseEnv, "al", "/r"); err == nil || !strings.Contains(err.Error(), "put back") {
		t.Errorf("edit of a config with a problem: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); string(b) != bad {
		t.Errorf("not put back:\n%s", b)
	}
	// A value that isn't a mapping is not overwritten.
	write(t, dir, "plinth:\n  agents:\n    enabled: [main]\n")
	if err := EnableAgent(work, ns, g, baseEnv, "al", "/r"); err == nil {
		t.Error("enabled: [main] was overwritten")
	}
}

// TestSetImage: image publish and image use write the pin by key, every other key and comment kept; the same pin
// again changes nothing; a pin that isn't one is refused before the file is touched.
func TestSetImage(t *testing.T) {
	work := t.TempDir()
	write(t, work, filled)
	g, err := LoadGlobal(work, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "michael", "devops")
	file := filepath.Join(dir, FileName)
	read := func() string {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	pin := ImagePin{Ref: pinRef, Commit: commit, Tree: tree}

	// The template: its text stays, the block is appended.
	tpl := string(workdir.NamespaceTemplate)
	write(t, dir, tpl)
	if err := SetImage(work, ns, g, baseEnv, pin); err != nil {
		t.Fatal(err)
	}
	if got := read(); !strings.HasPrefix(got, tpl) || !strings.Contains(got, "plinthctl image publish") {
		t.Errorf("the template after SetImage:\n%s", got)
	}
	if n, _, err := LoadNamespace(work, ns, g, baseEnv); err != nil || n.Image != pin {
		t.Errorf("pin = %+v, %v", n, err)
	}

	// A file with exec keys and comments: the pin goes beside them, and a new pin replaces the old one.
	write(t, dir, "plinth:\n  exec:\n    memory: 1g # big builds\n    image: # pinned\n      ref: x\n")
	if err := SetImage(work, ns, g, baseEnv, pin); err != nil {
		t.Fatal(err)
	}
	got := read()
	for _, want := range []string{"memory: 1g # big builds", "# pinned", "ref: " + pinRef, "commit: " + commit, "tree: " + tree} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	before := read()
	if err := SetImage(work, ns, g, baseEnv, pin); err != nil || read() != before {
		t.Errorf("the same pin again: %v, file changed: %v", err, read() != before)
	}
	other := ImagePin{Ref: "forgejo.example.org/michael/plinth-devops@sha256:" + strings.Repeat("a", 64), Commit: commit, Tree: tree}
	if err := SetImage(work, ns, g, baseEnv, other); err != nil {
		t.Fatal(err)
	}
	if n, _, err := LoadNamespace(work, ns, g, baseEnv); err != nil || n.Image != other || n.Memory != "1g" {
		t.Errorf("after a new pin: %+v, %v", n, err)
	}

	// Not a pin: refused, the file untouched.
	before = read()
	for _, bad := range []ImagePin{{}, {Ref: "forgejo.example.org/michael/plinth-devops:latest", Commit: commit, Tree: tree},
		{Ref: pinRef, Commit: "main", Tree: tree}} {
		if err := SetImage(work, ns, g, baseEnv, bad); err == nil || read() != before {
			t.Errorf("SetImage(%+v) = %v, file changed: %v", bad, err, read() != before)
		}
	}
}
