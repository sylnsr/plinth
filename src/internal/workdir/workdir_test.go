package workdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/safefs"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestTemplates(t *testing.T) {
	for name, tpl := range map[string][]byte{"global": GlobalTemplate, "namespace": NamespaceTemplate} {
		if len(tpl) == 0 || !strings.Contains(string(tpl), "plinth:") {
			t.Errorf("%s template is empty or has no plinth: key", name)
		}
		// A written template must work as it is, and agents are registered by plinthctl register, never in a config.
		for _, line := range strings.Split(string(tpl), "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && strings.Contains(l, "token-sha256") {
				t.Errorf("%s template registers an agent: %q", name, line)
			}
		}
	}
	// A new namespace must inherit every global default (e.g. the automatic reviews): nothing in its template may be set.
	for i, line := range strings.Split(string(NamespaceTemplate), "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			t.Errorf("namespace template line %d sets a key, which would hide the global value: %q", i+1, line)
		}
	}
}

func TestEnsureWork(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	created, err := EnsureWork(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "state", "state/.gitignore", "aaa-raw", "aaa-raw/.gitignore", "logs", "logs/.gitignore", "requests", "requests/.gitignore", "config.yml", BaseTemplateName} {
		if !slices.Contains(created, filepath.FromSlash(p)) {
			t.Errorf("%s not reported as created: %v", p, created)
		}
	}
	if m := mode(t, work); m != 0o700 {
		t.Errorf("<work> mode %v, want 0700", m)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "config.yml")); !bytes.Equal(b, GlobalTemplate) {
		t.Error("config.yml is not the global template")
	}
	if m := mode(t, filepath.Join(work, "config.yml")); m != 0o600 {
		t.Errorf("config.yml mode %v, want 0600", m)
	}
	if b, _ := os.ReadFile(filepath.Join(work, BaseTemplateName)); !bytes.Equal(b, BaseTemplate(doc.ModeAAA)) {
		t.Error("ask-template.yml is not the base template of mode aaa")
	}
	if b, _ := os.ReadFile(filepath.Join(work, "state", ".gitignore")); string(b) != gitignore {
		t.Errorf("state/.gitignore = %q", b)
	}
	// A second run creates nothing and keeps the user's config.
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := EnsureWork(work); err != nil || len(created) != 0 {
		t.Errorf("second EnsureWork = %v, %v; want nothing created", created, err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "config.yml")); string(b) != "mine\n" {
		t.Errorf("the user's config.yml was changed: %q", b)
	}
}

func TestEnsureNamespace(t *testing.T) {
	work := t.TempDir()
	ns := names.Namespace{Owner: "michael", Repo: "devops"}
	created, err := EnsureNamespace(work, ns)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(work, "michael", "devops")
	for _, d := range nsDirs {
		if m := mode(t, filepath.Join(base, d)); m != 0o700 {
			t.Errorf("%s mode %v, want 0700", d, m)
		}
		if b, err := os.ReadFile(filepath.Join(base, d, ".gitignore")); err != nil || string(b) != gitignore {
			t.Errorf("%s/.gitignore = %q, %v", d, b, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(base, "config.yml")); !bytes.Equal(b, NamespaceTemplate) {
		t.Error("config.yml is not the namespace template")
	}
	if b, _ := os.ReadFile(filepath.Join(base, BaseTemplateName)); !bytes.Equal(b, BaseTemplate(doc.ModePlinth)) {
		t.Error("ask-template.yml is not the base of mode plinth")
	}
	// owner, owner/repo, each directory and its .gitignore, config.yml, ask-template.yml
	if want := 2 + 2*len(nsDirs) + 2; len(created) != want {
		t.Errorf("created %d entries, want %d: %v", len(created), want, created)
	}

	// A second run creates nothing and keeps the user's config.
	if err := os.WriteFile(filepath.Join(base, "config.yml"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := EnsureNamespace(work, ns); err != nil || len(created) != 0 {
		t.Errorf("second EnsureNamespace = %v, %v; want nothing created", created, err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "config.yml")); string(b) != "mine\n" {
		t.Errorf("the user's config.yml was changed: %q", b)
	}

	// A missing directory is added on the next run.
	if err := os.RemoveAll(filepath.Join(base, "events")); err != nil {
		t.Fatal(err)
	}
	if created, err := EnsureNamespace(work, ns); err != nil || len(created) != 2 {
		t.Errorf("EnsureNamespace after events/ was removed = %v, %v; want events and its .gitignore", created, err)
	}
}

func TestEnsureNamespaceRefuses(t *testing.T) {
	work := t.TempDir()
	if _, err := EnsureNamespace(work, names.Namespace{Owner: "..", Repo: "x"}); err == nil {
		t.Error("EnsureNamespace accepted owner ..")
	}
	// A symlinked owner directory, even one inside <work>, is never followed.
	if err := os.Mkdir(filepath.Join(work, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(work, "michael")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureNamespace(work, names.Namespace{Owner: "michael", Repo: "devops"}); !errors.Is(err, safefs.ErrSymlink) {
		t.Errorf("EnsureNamespace through a symlink = %v, want ErrSymlink", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(work, "real")); len(entries) != 0 {
		t.Errorf("something was created through the symlink: %v", entries)
	}
}

func TestNamespaces(t *testing.T) {
	work := t.TempDir()
	if _, err := EnsureWork(work); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []names.Namespace{{Owner: "michael", Repo: "devops"}, {Owner: "ai", Repo: "docs"}} {
		if _, err := EnsureNamespace(work, ns); err != nil {
			t.Fatal(err)
		}
	}
	// Not namespaces: a repo directory with neither config.yml nor state/, a symlinked owner, <work>'s own directories.
	if err := os.MkdirAll(filepath.Join(work, "x", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("michael", filepath.Join(work, "linked")); err != nil {
		t.Fatal(err)
	}
	got, err := Namespaces(work)
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, ns := range got {
		s = append(s, ns.String())
	}
	if want := []string{"ai/docs", "michael/devops"}; !slices.Equal(s, want) {
		t.Errorf("Namespaces = %v, want %v", s, want)
	}
}

// register from several launch scripts and the controller starting may set up the same <work> at the same moment: all
// succeed, and each file is created (and reported) exactly once.
func TestEnsureConcurrent(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	ns := names.Namespace{Owner: "michael", Repo: "devops"}
	const n = 10
	created := make(chan []string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := EnsureWork(work)
			if err == nil {
				var c2 []string
				c2, err = EnsureNamespace(work, ns)
				c = append(c, c2...)
			}
			created <- c
			errs <- err
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent setup: %v", err)
		}
	}
	count := map[string]int{}
	for c := range created {
		for _, p := range c {
			count[p]++
		}
	}
	for _, p := range []string{"config.yml", filepath.Join("logs", ".gitignore"), filepath.Join("michael", "devops", "config.yml")} {
		if count[p] != 1 {
			t.Errorf("%s reported created %d times, want once", p, count[p])
		}
	}
}

// TestBaseTemplate: both base templates are AskTemplate manifests that decode strictly, each holding its mode's script
// as it is.
func TestBaseTemplate(t *testing.T) {
	for mode, script := range map[string][]byte{doc.ModeAAA: AAATemplate, doc.ModePlinth: AskTemplate} {
		tpl, err := doc.ParseAskTemplate(BaseTemplate(mode))
		if err != nil || tpl.Spec.Mode != mode || tpl.Metadata.Name != "base" || tpl.Spec.Script != string(script) {
			t.Errorf("%s: %+v %v", mode, tpl, err)
		}
	}
}
