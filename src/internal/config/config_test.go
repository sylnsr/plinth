package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"plinth/internal/names"
	"plinth/internal/workdir"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

// A pin: a digest, the commit and the tree of its .plinth/.
var (
	pinRef = "forgejo.example.org/michael/plinth-devops@sha256:" + strings.Repeat("d", 64)
	commit = strings.Repeat("c", 40)
	tree   = strings.Repeat("e", 40)
)

func env(vars map[string]string) Getenv {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

var baseEnv = env(map[string]string{"HOME": "/home/u", "FORGEJO_URL": "https://forgejo.example.org", "PLINTH_FORGEJO_TOKEN": "t0k"})

// filled is a global config with every placeholder of the template filled in.
const filled = `
forgejo-url: "${FORGEJO_URL}"
forgejo-token: "${PLINTH_FORGEJO_TOKEN}"
reviewers:
  type:
    security:
      urls:
        - "${FORGEJO_URL}/ai/docs/raw/branch/main/reviewers/security.md"
    dlp:
      urls:
        - "${FORGEJO_URL}/ai/docs/raw/branch/main/reviewers/dlp.md"
    code:
      definition: "Review the bash between <<<BEGIN SCRIPT>>> and <<<END SCRIPT>>>; ${VAR} stays as it is."
  automatic: [security, dlp]
ollama:
  host: "${OLLAMA_HOST}"
plinth:
  web-ui:
    listen: "192.168.122.66"
    public-url: "https://plinth.example.org:8790"
    auth:
      client-id: "abc"
      users: [michael]
  aaa-raw-dir: "~/aaa-raw"
  repo: "michael/plinth-scans"
`

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func problems(t *testing.T, err error) []string {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *config.Error", err)
	}
	return ce.Problems
}

func hasProblem(ps []string, sub string) bool {
	return slices.ContainsFunc(ps, func(p string) bool { return strings.Contains(p, sub) })
}

func TestFilledConfig(t *testing.T) {
	work := t.TempDir()
	write(t, work, filled)
	g, err := LoadGlobal(work, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	if g.ForgejoURL != "https://forgejo.example.org" || g.ForgejoToken != "t0k" {
		t.Errorf("${VAR} not expanded: %q %q", g.ForgejoURL, g.ForgejoToken)
	}
	if got := g.Reviewers.Type["security"].URLs[0]; got != "https://forgejo.example.org/ai/docs/raw/branch/main/reviewers/security.md" {
		t.Errorf("url = %q", got)
	}
	if d := g.Reviewers.Type["code"].Definition; !strings.Contains(d, "${VAR}") || !strings.Contains(d, "<<<BEGIN SCRIPT>>>") {
		t.Errorf("a definition must be taken literally: %q", d)
	}
	if g.Plinth.AAARawDir != "/home/u/aaa-raw" {
		t.Errorf("~ not expanded: %q", g.Plinth.AAARawDir)
	}
	// Defaults for what the file leaves out.
	if g.Ollama.TimeoutSeconds != 300 || g.Plinth.MCP.Port != 8791 || g.Plinth.Log.MaxAgeDays != 30 || g.Plinth.Messages.ExpirationMinutes != 60 {
		t.Errorf("defaults not applied: %+v %+v", g.Ollama, g.Plinth)
	}
	// OLLAMA_HOST is unset: empty, so Ollama is not configured, and a warning says so.
	if g.Ollama.Host != "" || !hasProblem(g.Warnings, "${OLLAMA_HOST} is not set") {
		t.Errorf("unset ${OLLAMA_HOST}: host %q, warnings %v", g.Ollama.Host, g.Warnings)
	}
	if want := []string{"FORGEJO_URL", "OLLAMA_HOST", "PLINTH_FORGEJO_TOKEN"}; !slices.Equal(g.EnvRefs, want) {
		t.Errorf("EnvRefs = %v, want %v", g.EnvRefs, want)
	}
	if sum := sha256.Sum256([]byte(filled)); g.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %s, not the file's", g.SHA256)
	}
}

// TestRepoAndAutosave: plinth.repo is read and checked, plinth.autosave defaults to false for both and reads
// true, and the retired plinth.failed-scans.repo is refused, naming plinth.repo.
func TestRepoAndAutosave(t *testing.T) {
	const repoLine = "  repo: \"michael/plinth-scans\"\n"
	if !strings.Contains(filled, repoLine) {
		t.Fatal("the filled config has no plinth.repo line; update this test")
	}
	load := func(body string) (*Global, error) {
		work := t.TempDir()
		write(t, work, body)
		return LoadGlobal(work, baseEnv)
	}
	g, err := load(filled)
	if err != nil {
		t.Fatal(err)
	}
	if g.Plinth.Repo != "michael/plinth-scans" || g.Plinth.Autosave.Runs || g.Plinth.Autosave.Failures {
		t.Fatalf("defaults: %+v", g.Plinth)
	}
	g, err = load(strings.Replace(filled, repoLine, repoLine+"  autosave:\n    runs: true\n    failures: true\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Plinth.Autosave.Runs || !g.Plinth.Autosave.Failures {
		t.Fatalf("autosave on: %+v", g.Plinth.Autosave)
	}
	_, err = load(strings.Replace(filled, repoLine, "  failed-scans:\n    repo: \"michael/plinth-scans\"\n", 1))
	if ps := problems(t, err); !hasProblem(ps, "plinth.failed-scans.repo: renamed to plinth.repo") {
		t.Errorf("the retired key: %q", ps)
	}
	_, err = load(strings.Replace(filled, repoLine, "  repo: \"nonsense\"\n", 1))
	if ps := problems(t, err); !hasProblem(ps, "plinth.repo:") {
		t.Errorf("a bad repo: %q", ps)
	}
}

func TestTemplateNeedsFillingIn(t *testing.T) {
	work := t.TempDir()
	write(t, work, string(workdir.GlobalTemplate))
	_, err := LoadGlobal(work, baseEnv)
	ps := problems(t, err)
	for _, key := range []string{"reviewers.type.security.urls[0]", "reviewers.type.code.definition", "ollama.models.review",
		"plinth.web-ui.listen", "plinth.web-ui.public-url", "plinth.web-ui.auth.client-id", "plinth.web-ui.auth.users[0]",
		"plinth.messages.catalogue-url", "plinth.aaa-raw-dir", "plinth.repo"} {
		if !hasProblem(ps, key+": still holds the template placeholder") {
			t.Errorf("no placeholder problem for %s in:\n%s", key, strings.Join(ps, "\n"))
		}
	}
	// Nothing else is wrong with the template, and a key holding a placeholder reports only that (no follow-on noise
	// such as "<address>" is not an IP address).
	for _, p := range ps {
		if !strings.Contains(p, "still holds the template placeholder") {
			t.Errorf("a problem in the template that is not a placeholder: %s", p)
		}
	}
}

func TestStrict(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"forgejo-url: https://f.example.org\nforgejo-urll: x\n", "field forgejo-urll not found"},
		{"forgejo-url: https://f.example.org\nplinth:\n  runner:\n    poll-seconds: often\n", "cannot unmarshal"},
		{"forgejo-url: https://f.example.org\nplinth:\n  runner:\n    poll-seconds: 0\n", "plinth.runner.poll-seconds: 0 is outside 1..60"},
		{"forgejo-url: https://f.example.org\nplinth:\n  worker:\n    poll-seconds: 1\n", "field worker not found"}, // the name in older versions
		{"forgejo-url: https://f.example.org\nreviewers:\n  automatic: [security]\n", `review type "security" is not defined`},
		{"forgejo-url: ftp://f.example.org\n", "forgejo-url: not an http(s) URL"},
		{"forgejo-url: https://user:pw@f.example.org\n", "no user:password"},
		{"forgejo-token: x\n", "forgejo-url: required"},
		{"forgejo-url: https://f.example.org\nplinth:\n  mcp:\n    port: 8790\n", "plinth.web-ui.port: 8790 is also plinth.mcp.port"},
		{"forgejo-url: https://f.example.org\nollama:\n  host: http://o:11434\n", "ollama.models.review and .dlp: both required"},
		{"forgejo-url: https://f.example.org\nollama:\n  mcp:\n    port: 8082\n", "field mcp not found"}, // the Ollama MCP has no address of its own
		{"forgejo-url: https://f.example.org\nplinth:\n  messages:\n    ungated:\n      agents: [\"plinth_abc\"]\n", "agents[0]: not an agent name"},
		{"forgejo-url: https://f.example.org\n---\nforgejo-url: https://g.example.org\n", "more than one YAML document"},
		{"forgejo-url: [unclosed\n", "did not find expected"},
	} {
		work := t.TempDir()
		write(t, work, c.body)
		_, err := LoadGlobal(work, baseEnv)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want %q", c.body, err, c.want)
		}
	}
	// Every problem at once.
	work := t.TempDir()
	write(t, work, "forgejo-url: x\nplinth:\n  runner:\n    poll-seconds: 0\n    max-parallel-exec: 99\n")
	_, err := LoadGlobal(work, baseEnv)
	if ps := problems(t, err); len(ps) != 3 {
		t.Errorf("want 3 problems at once, got %d: %v", len(ps), ps)
	}
	// A ${VAR} that is unset (or empty) names itself in the problems it causes.
	work = t.TempDir()
	write(t, work, filled)
	_, err = LoadGlobal(work, env(map[string]string{"HOME": "/home/u", "PLINTH_FORGEJO_TOKEN": "t0k"}))
	ps := problems(t, err)
	if !hasProblem(ps, "forgejo-url: required (${FORGEJO_URL} is not set)") ||
		!hasProblem(ps, "reviewers.type.security.urls[0]: not an http(s) URL") || !hasProblem(ps, "user:password (the value isn't shown: it may hold a password) (${FORGEJO_URL} is not set)") {
		t.Errorf("an unset ${FORGEJO_URL} isn't named:\n%s", strings.Join(ps, "\n"))
	}
	_, err = LoadGlobal(work, env(map[string]string{"HOME": "/home/u", "FORGEJO_URL": ""}))
	if ps := problems(t, err); !hasProblem(ps, "forgejo-url: required (${FORGEJO_URL} is empty)") {
		t.Errorf("an empty ${FORGEJO_URL} isn't named:\n%s", strings.Join(ps, "\n"))
	}
	// A global config without automatic reviews loads, with a warning, and so does a namespace that inherits it.
	work = t.TempDir()
	write(t, work, "forgejo-url: https://f.example.org\n")
	g, err := LoadGlobal(work, baseEnv)
	if err != nil || !hasProblem(g.Warnings, "reviewers.automatic is empty") {
		t.Fatalf("no automatic reviews in the global config: %v, %v", g, err)
	}
	if _, warns, err := LoadNamespace(work, ns, g, baseEnv); err != nil || !hasProblem(warns, "(set in the global config)") {
		t.Errorf("namespace inheriting no automatic reviews: %v, %v", warns, err)
	}
	// A missing file and a symlinked one.
	if _, err := LoadGlobal(t.TempDir(), baseEnv); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing config: %v", err)
	}
	if _, err := LoadGlobal(filepath.Join(t.TempDir(), "nope"), baseEnv); err == nil || !strings.Contains(err.Error(), "missing, and so is its directory") {
		t.Errorf("missing work directory: %v", err)
	}
	work = t.TempDir()
	write(t, filepath.Join(work, "real"), "forgejo-url: https://f.example.org\n")
	if err := os.Symlink(filepath.Join("real", FileName), filepath.Join(work, FileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(work, baseEnv); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked config: %v", err)
	}
}

func TestNamespace(t *testing.T) {
	work := t.TempDir()
	write(t, work, filled)
	g, err := LoadGlobal(work, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "michael", "devops")

	// The template sets nothing: every key comes from the global config.
	write(t, dir, string(workdir.NamespaceTemplate))
	n, _, err := LoadNamespace(work, ns, g, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.Automatic, []string{"security", "dlp"}) || n.ExpirationMinutes != 60 || n.From["reviewers.automatic"] != "global" {
		t.Errorf("template namespace = %+v", n)
	}
	if _, warns, _ := LoadNamespace(work, ns, g, baseEnv); hasProblem(warns, "reviewers.automatic") || hasProblem(g.Warnings, "reviewers.automatic") {
		t.Errorf("a warning about automatic reviews that are there: %v %v", warns, g.Warnings)
	}
	// No config.yml at all: the same; its SHA256 is that of nothing.
	if n, _, err := LoadNamespace(work, names.Namespace{Owner: "ai", Repo: "kb"}, g, baseEnv); err != nil || len(n.Automatic) != 2 ||
		n.SHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("no namespace config: %+v, %v", n, err)
	}

	// automatic: [] turns the reviews off for this repo; the other keys still come from the global config.
	write(t, dir, "reviewers:\n  automatic: []\nplinth:\n  messages:\n    expiration-minutes: 15\n")
	n, _, err = LoadNamespace(work, ns, g, baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Automatic) != 0 || n.From["reviewers.automatic"] != "namespace" || n.ExpirationMinutes != 15 || n.From["plinth.messages.ungated.agents"] != "global" {
		t.Errorf("namespace overrides = %+v", n)
	}
	// ... which is legitimate, but warned about.
	if _, warns, _ := LoadNamespace(work, ns, g, baseEnv); !hasProblem(warns, "reviewers.automatic is empty") || !hasProblem(warns, "(set in the namespace config)") {
		t.Errorf("automatic: [] gives no warning: %v", warns)
	}
	// A list replaces the global list, never merges with it.
	write(t, dir, "reviewers:\n  automatic: [code]\n")
	if n, _, err = LoadNamespace(work, ns, g, baseEnv); err != nil || !slices.Equal(n.Automatic, []string{"code"}) {
		t.Errorf("automatic [code] = %+v, %v", n, err)
	}
	// plinth.exec: global defaults (no env file), replaced per namespace.
	if n.EnvFile != "" || n.From["plinth.exec.env-file"] != "global" {
		t.Errorf("exec defaults = %q, %q", n.EnvFile, n.From["plinth.exec.env-file"])
	}
	if n.Memory != "512m" || n.From["plinth.exec.memory"] != "global" {
		t.Errorf("memory default = %q, %q", n.Memory, n.From["plinth.exec.memory"])
	}
	if n.Image != (ImagePin{}) || n.From["plinth.exec.image"] != "namespace" {
		t.Errorf("a pin without one: %+v, %q", n.Image, n.From["plinth.exec.image"])
	}
	write(t, dir, "plinth:\n  exec:\n    image:\n      ref: "+pinRef+"\n      commit: "+commit+"\n      tree: "+tree+"\n")
	if n, _, err = LoadNamespace(work, ns, g, baseEnv); err != nil || n.Image.Ref != pinRef || n.Image.Commit != commit ||
		n.Image.Tree != tree || n.Image.Name() != "forgejo.example.org/michael/plinth-devops" || n.Image.Digest() != "sha256:"+strings.Repeat("d", 64) {
		t.Errorf("pin = %+v, %v", n.Image, err)
	}
	write(t, work, strings.Replace(filled, "plinth:\n", "plinth:\n  exec:\n    image:\n      ref: "+pinRef+"\n", 1))
	if _, err := LoadGlobal(work, baseEnv); err == nil || !strings.Contains(err.Error(), "field image not found") {
		t.Errorf("a pin in the global config: %v", err) // namespace only
	}
	write(t, work, filled)
	// The retired plinth.exec.image-ref (the build from the checkout): still read, so an old config loads, and warned
	// about at both levels.
	write(t, dir, "plinth:\n  exec:\n    image-ref: release/1\n")
	if _, warns, err := LoadNamespace(work, ns, g, baseEnv); err != nil || !hasProblem(warns, "plinth.exec.image-ref is no longer used") {
		t.Errorf("image-ref in a namespace: %v, %v", warns, err)
	}
	write(t, work, strings.Replace(filled, "plinth:\n", "plinth:\n  exec:\n    image-ref: \"\"\n", 1))
	if g2, err := LoadGlobal(work, baseEnv); err != nil || !hasProblem(g2.Warnings, "plinth.exec.image-ref is no longer used") {
		t.Errorf("image-ref in the global config: %v", err)
	}
	write(t, work, filled)
	write(t, dir, "plinth:\n  exec:\n    env-file: exec.env\n    network: true\n    memory: 1g\n")
	if n, _, err = LoadNamespace(work, ns, g, baseEnv); err != nil || n.EnvFile != "exec.env" || !n.Network || n.Privileged ||
		n.From["plinth.exec.env-file"] != "namespace" || n.Memory != "1g" || n.From["plinth.exec.memory"] != "namespace" {
		t.Errorf("exec = %+v, %v", n, err)
	}

	for _, c := range []struct{ body, want string }{
		{"forgejo-url: https://other.example.org\n", "field forgejo-url not found"},               // a global-only key
		{"plinth:\n  runner:\n    poll-seconds: 2\n", "field runner not found"},                   // likewise
		{"reviewers:\n  automatic: [nope]\n", `review type "nope" is not defined`},                // checked against the global types
		{"plinth:\n  messages:\n    expiration-minutes: 0\n", "expiration-minutes: 0 is outside"}, // range
		{"plinth:\n  exec:\n    env-file: ../exec.env\n", "env-file: \"../exec.env\" is not a file name"},
		{"plinth:\n  exec:\n    env-file: config.yml\n", "is not a file name"},
		{"plinth:\n  exec:\n    memory: 512\n", "memory: \"512\" is not <n>k"},
		{"plinth:\n  exec:\n    memory: 2m\n", "memory: 2m is not between 6m and 64g"},
		{"plinth:\n  exec:\n    memory: 65g\n", "is not between"},
		{"plinth:\n  exec:\n    memory: 1t\n", "is not <n>k"},
		{"plinth:\n  exec:\n    ssh-key: ~/.ssh/plinth\n", "field ssh-key not found"},
		// The pin: a digest, never a tag; all three keys; nothing else in it.
		{"plinth:\n  exec:\n    image:\n      ref: forgejo.example.org/michael/plinth-devops:latest\n      commit: " + commit + "\n      tree: " + tree + "\n",
			"plinth.exec.image: ref \"forgejo.example.org/michael/plinth-devops:latest\" is not <name>@sha256:<64 hex>"},
		{"plinth:\n  exec:\n    image:\n      ref: " + pinRef + "\n      tree: " + tree + "\n", "plinth.exec.image: commit \"\" is not a git commit id"},
		{"plinth:\n  exec:\n    image:\n      ref: " + pinRef + "\n      commit: " + commit + "\n      tree: main\n", "tree \"main\" is not a git tree id"},
		{"plinth:\n  exec:\n    image:\n      ref: " + pinRef + "\n      commit: " + commit + "\n      tree: " + tree + "\n      tag: x\n", "field tag not found"},
	} {
		write(t, dir, c.body)
		if _, _, err := LoadNamespace(work, ns, g, baseEnv); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want %q", c.body, err, c.want)
		}
	}
}

func TestProblemsNeverQuoteSecrets(t *testing.T) {
	work := t.TempDir()
	write(t, work, "forgejo-url: https://user:pw-s3cret@f.example.org\nplinth:\n  messages:\n    ungated:\n      agents: [\"plinth_tok3n\"]\n")
	_, err := LoadGlobal(work, baseEnv)
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("problems must name the key, never the value: %v", err)
	}
}

// TestRawDir: the plain-AAA commands read plinth.aaa-raw-dir alone: no file is the default, the rest of the file may
// be incomplete, and only that key's problems count.
func TestRawDir(t *testing.T) {
	work := t.TempDir()
	if d, err := RawDir(work, baseEnv); err != nil || d != filepath.Join(work, "aaa-raw") {
		t.Errorf("no config.yml: %q, %v", d, err)
	}
	// No forgejo-url, an unset ${VAR}, a key of the template still unfilled: none of them matter here.
	write(t, work, "ollama:\n  host: \"${PLINTH_UNSET_FOR_TEST}\"\nplinth:\n  aaa-raw-dir: \"~/raw\"\n  repo: \"<owner>/<repo>\"\n")
	if d, err := RawDir(work, baseEnv); err != nil || d != "/home/u/raw" {
		t.Errorf("~/raw: %q, %v", d, err)
	}
	write(t, work, "plinth:\n  log:\n    max-age-days: 30\n")
	if d, err := RawDir(work, baseEnv); err != nil || d != filepath.Join(work, "aaa-raw") {
		t.Errorf("without the key: %q, %v", d, err)
	}
	for _, c := range []struct{ body, want string }{
		{"plinth:\n  aaa-raw-dir: \"raw\"\n", "is not an absolute path"},
		{"plinth:\n  aaa-raw-dir: \"<path>\"\n", "still holds the template placeholder"},
		{"plinth: [\n", ""},
		{"plinth:\n  aaa-raw-dir: \"a\"\n---\nb: 1\n", "more than one YAML document"},
	} {
		write(t, work, c.body)
		if _, err := RawDir(work, baseEnv); err == nil || !strings.Contains(err.Error()+strings.Join(problems(t, err), "\n"), c.want) {
			t.Errorf("%q: %v, want %q", c.body, err, c.want)
		}
	}
}

// TestLoadPlain: dlp-scan and review read the ollama: and reviewers: keys too, and only the problems of the keys they
// ask for count.
func TestLoadPlain(t *testing.T) {
	work := t.TempDir()
	pl, err := LoadPlain(work, baseEnv, OllamaKey)
	if err != nil || pl.Ollama.Host != "" || pl.Ollama.TimeoutSeconds != 300 || pl.AAARawDir != filepath.Join(work, "aaa-raw") {
		t.Fatalf("no config.yml: %+v, %v", pl, err)
	}
	write(t, work, "ollama:\n  host: http://o.example:11434/\n  models:\n    review: r\n    dlp: d\n  retry-seconds: 0\n"+
		"reviewers:\n  type:\n    code:\n      definition: \"Review ${it}\"\nplinth:\n  repo: \"<owner>/<repo>\"\n")
	pl, err = LoadPlain(work, baseEnv, OllamaKey, ReviewersKey)
	if err != nil || pl.Ollama.Host != "http://o.example:11434/" || pl.Ollama.Models.DLP != "d" || pl.Ollama.RetrySeconds != 0 ||
		pl.Reviewers.Type["code"].Definition != "Review ${it}" {
		t.Fatalf("%+v, %v", pl, err)
	}
	write(t, work, "ollama:\n  host: http://o.example:11434\n")
	if _, err := LoadPlain(work, baseEnv); err != nil {
		t.Errorf("scrubber doesn't read ollama: %v", err)
	}
	if _, err := LoadPlain(work, baseEnv, OllamaKey); err == nil || !strings.Contains(strings.Join(problems(t, err), "\n"), "ollama.models.review and .dlp") {
		t.Errorf("a host without models: %v", err)
	}

	// An unknown key below a key of theirs stops the plain commands too; one elsewhere doesn't.
	models := "ollama:\n  host: http://o.example:11434\n  models:\n    review: r\n    dlp: d\n"
	for _, c := range []struct {
		cfg, want string
		keys      []string
	}{
		{models + "  mcp:\n    listen: 127.0.0.1\n", "ollama.mcp: field mcp not found in type config.Ollama (line 6)", []string{OllamaKey}},
		{models + "  max-paralel: 2\n", "ollama.max-paralel: field max-paralel not found", []string{OllamaKey}},
		{models + "  timeout-seconds: soon\n", "ollama.timeout-seconds: cannot unmarshal", []string{OllamaKey}},
		{"reviewers:\n  type:\n    code:\n      definiton: x\n", "reviewers.type.code.definiton: field definiton not found", []string{ReviewersKey}},
		{models + "plinth:\n  frobnicate: 1\n", "", []string{OllamaKey}},
		{models + "  mcp:\n    port: 8082\n", "", nil}, // scrubber reads no ollama:
	} {
		write(t, work, c.cfg)
		_, err := LoadPlain(work, baseEnv, c.keys...)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q: %v", c.cfg, err)
		case c.want != "" && (err == nil || !strings.Contains(strings.Join(problems(t, err), "\n"), c.want)):
			t.Errorf("%q: %v, want %q", c.cfg, err, c.want)
		}
	}
}

// TestStrictLines: a strict-decode problem names its key path and the key's line in the file, not a line of the
// expanded document the decoder saw (a ${VAR} or a comment block shifts those).
func TestStrictLines(t *testing.T) {
	work := t.TempDir()
	write(t, work, "# the global config\n\n# more comments\nforgejo-url: \"${FORGEJO_URL}\"\n\nplinth:\n  # a comment\n  runner:\n    poll-seconds: often\n  bogus: 1\n")
	_, err := LoadGlobal(work, baseEnv)
	got := strings.Join(problems(t, err), "\n")
	for _, want := range []string{"plinth.runner.poll-seconds: cannot unmarshal", "(line 9)", "plinth.bogus: field bogus not found in type", "(line 10)"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems %q lack %q", got, want)
		}
	}
	if strings.HasPrefix(got, "line ") || strings.Contains(got, "\nline ") {
		t.Errorf("a line of the expanded document: %q", got)
	}
}

// TestNoticeExpires: plinth.notice.expires defaults to 14 days, is read, and may not exceed the log's age limit.
func TestNoticeExpires(t *testing.T) {
	load := func(body string) (*Global, error) {
		work := t.TempDir()
		write(t, work, body)
		return LoadGlobal(work, baseEnv)
	}
	g, err := load(filled)
	if err != nil {
		t.Fatal(err)
	}
	if g.Plinth.Notice.Expires != 14 {
		t.Fatalf("default: %+v", g.Plinth.Notice)
	}
	g, err = load(strings.Replace(filled, "plinth:\n", "plinth:\n  notice:\n    expires: 7\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if g.Plinth.Notice.Expires != 7 {
		t.Fatalf("7 days: %+v", g.Plinth.Notice)
	}
	for _, bad := range []string{"0", "31", "4000"} {
		_, err = load(strings.Replace(filled, "plinth:\n", "plinth:\n  notice:\n    expires: "+bad+"\n", 1))
		if ps := problems(t, err); !hasProblem(ps, "plinth.notice.expires") {
			t.Errorf("expires %s: %q", bad, ps)
		}
	}
}

// TestPreviewLimit: plinth.preview.limit defaults to 10 calls per hour, is read, and is between 1 and 1000.
func TestPreviewLimit(t *testing.T) {
	load := func(body string) (*Global, error) {
		work := t.TempDir()
		write(t, work, body)
		return LoadGlobal(work, baseEnv)
	}
	g, err := load(filled)
	if err != nil {
		t.Fatal(err)
	}
	if g.Plinth.Preview.Limit != 60 {
		t.Fatalf("default: %+v", g.Plinth.Preview)
	}
	g, err = load(strings.Replace(filled, "plinth:\n", "plinth:\n  preview:\n    limit: 3\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if g.Plinth.Preview.Limit != 3 {
		t.Fatalf("3 calls: %+v", g.Plinth.Preview)
	}
	for _, bad := range []string{"0", "1001"} {
		_, err = load(strings.Replace(filled, "plinth:\n", "plinth:\n  preview:\n    limit: "+bad+"\n", 1))
		if ps := problems(t, err); !hasProblem(ps, "plinth.preview.limit") {
			t.Errorf("limit %s: %q", bad, ps)
		}
	}
}
