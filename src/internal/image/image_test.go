package image

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/forgejo"
	"plinth/internal/gitrepo"
	"plinth/internal/imagetest"
	"plinth/internal/names"
)

func TestRepoAndTag(t *testing.T) {
	repo, host, err := Repo("https://Forgejo.Example.org:3000/sub", names.Namespace{Owner: "Michael", Repo: "DevOps"})
	if err != nil || repo != "forgejo.example.org:3000/michael/plinth-devops" || host != "forgejo.example.org:3000" {
		t.Errorf("Repo = %q, %q, %v", repo, host, err)
	}
	if _, _, err := Repo("not a url", names.Namespace{Owner: "m", Repo: "r"}); err == nil {
		t.Error("Repo of no URL")
	}
	at := time.Date(2026, 10, 16, 12, 30, 5, 0, time.FixedZone("x", -5*3600))
	if got := Tag("0123456789abcdef0123456789abcdef01234567", at); got != "0123456789ab-20261016T173005Z" {
		t.Errorf("Tag = %q", got)
	}
}

func TestClean(t *testing.T) {
	var b bytes.Buffer
	Clean(&b).Write([]byte("a\x1b[31mb\r\nc\td\x07"))
	if b.String() != "a?[31mb?\nc\td?" {
		t.Errorf("Clean = %q", b.String())
	}
}

// TestParseRef: a tag or a digest of the namespace's repo; <name>@<digest> only where an image published elsewhere is
// allowed (image use).
func TestParseRef(t *testing.T) {
	const repo = "f.example.org/michael/plinth-devops"
	d := "sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		arg       string
		elsewhere bool
		want      Ref
		err       string
	}{
		{"0123456789ab-20261016T173005Z", false, Ref{Name: repo, Tag: "0123456789ab-20261016T173005Z"}, ""},
		{d, false, Ref{Name: repo, Digest: d}, ""},
		{"other.example.org/x/y@" + d, true, Ref{Name: "other.example.org/x/y", Digest: d}, ""},
		{"other.example.org/x/y@" + d, false, Ref{}, "by its tag or sha256:<digest>"},
		{"other.example.org/x/y:latest@" + d, true, Ref{Name: "other.example.org/x/y:latest", Digest: d}, ""},
		{"x@sha256:abc", true, Ref{}, "is not <name>@sha256:<64 hex>"},
		{"sha256:abc", false, Ref{}, "is not sha256:<64 hex>"},
		{"-x", false, Ref{}, "is not a tag"},
		{"a/b", false, Ref{}, "is not a tag"},
	} {
		got, err := ParseRef(c.arg, repo, c.elsewhere)
		if got != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("ParseRef(%q, %v) = %+v, %v; want %+v, %q", c.arg, c.elsewhere, got, err, c.want, c.err)
		}
	}
	if r := (Ref{Name: repo, Tag: "t"}); r.String() != repo+":t" || r.Host() != "f.example.org" {
		t.Errorf("%+v: %q %q", r, r.String(), r.Host())
	}
}

// TestPublish: publish fetches the default branch from Forgejo, checks .plinth/ and diffs it against the pin; it builds,
// pushes with the user's registry login, hands over the pin and pulls the image back, so podman finds it by its digest
// and runs it with --pull=never, which is how the controller will; a later publish diffs against the pin; a refused
// Containerfile is never built; one publish of a namespace at a time; the fetched repo goes afterwards. Then what use,
// list and delete call: the tags, an image's labels as a pin (only for this namespace), this machine's copy, and
// the delete from the registry.
func TestPublish(t *testing.T) {
	img := imagetest.NeedTools(t)
	ns := names.Namespace{Owner: "michael", Repo: "publish-test-" + strconv.Itoa(os.Getpid())}
	f := imagetest.NewForge(t, ns, "plinth-token")
	repo := f.Repo()
	t.Cleanup(func() { imagetest.RemoveImages(ns) })
	f.Trust(t)
	f.Logout(t)
	if err := LoggedIn(f.Host, ns); err == nil || !strings.Contains(err.Error(), "not logged in to "+f.Host+": run plinthctl image login -n "+ns.String()) {
		t.Errorf("not logged in: %v", err)
	}
	f.Login(t)
	if err := LoggedIn(f.Host, ns); err != nil {
		t.Fatal(err)
	}
	head := f.Commit(t, "FROM "+img+"\nRUN echo one > /built.txt\n", "image one")

	fj, err := forgejo.New(f.URL, f.Token, f.CA)
	if err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(t.TempDir(), "michael", ns.Repo)
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := Prepare(ctx, fj, nsDir, ns, config.ImagePin{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Branch != "main" || plan.Commit != head || plan.Repo != repo || !plan.Protection.Protected || plan.DiffFrom != "" ||
		!strings.Contains(plan.Diff, "+FROM "+img) || len(plan.Refused()) != 0 || len(plan.Sum) != 64 {
		t.Fatalf("first plan: %+v", plan)
	}
	if _, err := Prepare(ctx, fj, nsDir, ns, config.ImagePin{}, ""); err == nil || !strings.Contains(err.Error(), "another image publish") {
		t.Errorf("a second publish at once: %v", err)
	}
	var pinned config.ImagePin
	var progress bytes.Buffer
	pub, err := plan.Publish(ctx, time.Now(), &progress, func(p config.ImagePin) error {
		pinned = p
		return nil
	})
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, progress.String())
	}
	if pinned != pub.Pin || pub.Pin.Ref != repo+"@"+pub.Digest || pub.Pin.Commit != head || pub.Pin.Tree != plan.Tree ||
		!strings.HasPrefix(pub.Tag, head[:12]+"-") {
		t.Errorf("published %+v, pinned %+v", pub, pinned)
	}
	if d := f.Reg.ManifestDigest("michael/plinth-"+ns.Repo, pub.Tag); d != pub.Digest {
		t.Errorf("the registry has %q under the tag, want %s", d, pub.Digest)
	}
	if err := plan.Close(); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(nsDir, "state", "git")); !os.IsNotExist(err) {
		t.Errorf("<ns-dir>/state/git/ is still there: %v", err)
	}
	// This machine has exactly the pinned image: found by its digest, with the labels, and it runs with --pull=never.
	// The test image has no Ansible: its label says so.
	out, err := exec.Command("podman", "image", "inspect", "--format",
		`{{index .Labels "org.opencontainers.image.revision"}} {{index .Labels "plinth.tree"}} {{index .Labels "plinth.image"}} {{index .Labels "plinth.ansible-core"}}`,
		pub.Pin.Ref).Output()
	if err != nil || strings.TrimSpace(string(out)) != head+" "+plan.Tree+" "+ns.String()+" none" || pub.Ansible != "none" {
		t.Errorf("labels of %s: %q, %v; ansible-core %q", pub.Pin.Ref, out, err, pub.Ansible)
	}
	// The build before the label went: no untagged image of the namespace is left.
	if out, err := exec.Command("podman", "images", "--quiet", "--filter", "dangling=true", "--filter",
		"label=plinth.image="+ns.String()).Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("untagged images left: %q, %v", out, err)
	}
	if out, err := exec.Command("podman", "run", "--rm", "--pull=never", pub.Pin.Ref, "cat", "/built.txt").Output(); err != nil || string(out) != "one\n" {
		t.Errorf("running the pin: %q, %v", out, err)
	}
	if d, err := Resolve(ctx, repo, pub.Tag); err != nil || d != pub.Digest {
		t.Errorf("Resolve(%s) = %q, %v", pub.Tag, d, err)
	}
	// Pulled again by its digest alone, after this machine's copy went.
	exec.Command("podman", "rmi", "--force", pub.Pin.Ref).Run()
	if err := Pull(ctx, repo, pub.Digest, ""); err != nil {
		t.Errorf("pull by digest: %v", err)
	}

	// A later publish diffs against the pin; a pinned commit that isn't on the branch shows every file.
	f.Commit(t, "FROM "+img+"\nRUN echo two > /built.txt\n", "image two")
	plan, err = Prepare(ctx, fj, nsDir, ns, pub.Pin, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DiffFrom != head || !strings.Contains(plan.Diff, "-RUN echo one") || !strings.Contains(plan.Diff, "+RUN echo two") ||
		strings.Contains(plan.Diff, "+FROM") {
		t.Errorf("diff against the pin (%s):\n%s", plan.DiffFrom, plan.Diff)
	}
	plan.Close()
	gone := pub.Pin
	gone.Commit = strings.Repeat("f", 40)
	if plan, err = Prepare(ctx, fj, nsDir, ns, gone, ""); err != nil || plan.DiffFrom != "" || !strings.Contains(plan.DiffNote, "is not on main") ||
		!strings.Contains(plan.Diff, "+FROM") {
		t.Errorf("a pinned commit off the branch: %+v, %v", plan, err)
	}
	plan.Close()
	// The first commit, named: on the branch, so it is taken; one that isn't, isn't.
	if plan, err = Prepare(ctx, fj, nsDir, ns, config.ImagePin{}, head); err != nil || plan.Commit != head {
		t.Errorf("publish of %s: %+v, %v", head, plan, err)
	}
	plan.Close()
	if _, err := Prepare(ctx, fj, nsDir, ns, config.ImagePin{}, strings.Repeat("f", 40)); err == nil || !strings.Contains(err.Error(), "is not on main") {
		t.Errorf("a commit off the branch: %v", err)
	}

	// A refused Containerfile: shown, never built.
	f.Commit(t, "FROM docker.io/library/debian:12\n", "unpinned")
	if plan, err = Prepare(ctx, fj, nsDir, ns, pub.Pin, ""); err != nil || len(plan.Refused()) == 0 {
		t.Fatalf("unpinned FROM: %+v, %v", plan, err)
	}
	noPin := func(config.ImagePin) error {
		t.Error("a refused plan was pinned")
		return nil
	}
	if _, err := plan.Publish(ctx, time.Now(), nil, noPin); err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Errorf("publish of a refused plan: %v", err)
	}
	plan.Close()

	// A base image with ONBUILD triggers (Docker format keeps them) is refused before the build; this machine doesn't
	// have it by its digest, so its config comes from the registry.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+img+"\nONBUILD RUN echo triggered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	iid := filepath.Join(t.TempDir(), "iid")
	if out, err := exec.Command("podman", "build", "--format", "docker", "--pull=never", "--iidfile", iid, dir).CombinedOutput(); err != nil {
		t.Fatalf("the ONBUILD base: %v\n%s", err, out)
	}
	b, err := os.ReadFile(iid)
	if err != nil {
		t.Fatal(err)
	}
	baseID := strings.TrimPrefix(strings.TrimSpace(string(b)), "sha256:")
	defer exec.Command("podman", "rmi", baseID).Run()
	baseDigest, err := push(ctx, baseID, repo+":onbuild-base")
	if err != nil {
		t.Fatal(err)
	}
	f.Commit(t, "FROM "+repo+"@"+baseDigest+"\nRUN true\n", "an ONBUILD base")
	plan, err = Prepare(ctx, fj, nsDir, ns, pub.Pin, "")
	if err != nil {
		t.Fatalf("an ONBUILD base: %v", err)
	}
	if r := plan.Refused(); len(r) != 1 || !strings.Contains(r[0].Text, "has ONBUILD triggers, which the build would run unchecked: RUN echo triggered") {
		t.Errorf("an ONBUILD base (%s@%s, here: %v), %d findings:", repo, baseDigest, Present(repo+"@"+baseDigest), len(plan.Findings))
		for _, f := range plan.Findings {
			t.Errorf("  %s", f)
		}
	}
	plan.Close()

	// What use, list and delete call.
	if tags, err := Tags(ctx, repo); err != nil || !slices.Contains(tags, pub.Tag) {
		t.Errorf("Tags = %v, %v", tags, err)
	}
	info, err := Inspect(ctx, repo+":"+pub.Tag)
	if err != nil || info.Digest != pub.Digest || info.Created.IsZero() {
		t.Fatalf("Inspect = %+v, %v", info, err)
	}
	if p, err := PinOf(info, repo, ns); err != nil || p != pub.Pin {
		t.Errorf("PinOf = %+v, %v; want %+v", p, err, pub.Pin)
	}
	other := names.Namespace{Owner: "michael", Repo: "other"}
	if _, err := PinOf(info, repo, other); err == nil || !strings.Contains(err.Error(), "not a Plinth image of michael/other") {
		t.Errorf("PinOf for another namespace: %v", err)
	}
	if !SameLocal(pub.Pin.Ref, pub.Pin.Ref) || SameLocal(pub.Pin.Ref, img) || SameLocal(pub.Pin.Ref, "") {
		t.Error("SameLocal")
	}
	if err := RemoveLocal(pub.Pin.Ref); err != nil || Present(pub.Pin.Ref) {
		t.Errorf("RemoveLocal: %v, still here: %v", err, Present(pub.Pin.Ref))
	}
	if err := Delete(ctx, repo, pub.Digest); err != nil {
		t.Fatal(err)
	}
	if d := f.Reg.ManifestDigest("michael/plinth-"+ns.Repo, pub.Tag); d != "" {
		t.Errorf("the tag still names %s after the delete", d)
	}
	if _, err := Inspect(ctx, repo+"@"+pub.Digest); err == nil {
		t.Error("Inspect of a deleted image")
	}
}

// TestAnsibleVersion: the version comes from ansible --version's first line, as ansible-core prints it; anything else
// is none.
func TestAnsibleVersion(t *testing.T) {
	for _, c := range [][2]string{
		{"ansible [core 2.19.11]\n  config file = None\n", "2.19.11"},
		{"ansible [core 2.16.3]\n", "2.16.3"},
		{"ansible 2.9.6\n", ""},
		{"bash: ansible: command not found\n", ""},
		{"ansible [core 2.19.11\"; rm -rf /]\n", ""},
	} {
		got := ""
		if m := ansibleRe.FindStringSubmatch(c[0]); m != nil {
			got = m[1]
		}
		if got != c[1] {
			t.Errorf("%q: %q, want %q", c[0], got, c[1])
		}
	}
}

// TestAnsibleVersionEntrypoint: an image with an ENTRYPOINT of its own still gives ansible's version, and is built in
// OCI format, which has no ONBUILD (devops review of a6e9ad6).
func TestAnsibleVersionEntrypoint(t *testing.T) {
	img := imagetest.NeedTools(t)
	files := []gitrepo.File{
		{Path: container.Containerfile, Data: []byte("FROM " + img + "\nCOPY ansible /usr/local/bin/ansible\nENTRYPOINT [\"echo\", \"entry\"]\n")},
		{Path: "ansible", Exec: true, Data: []byte("#!/bin/sh\n[ \"$*\" = --version ] && echo 'ansible [core 9.9.9]'\n")},
	}
	ctx := context.Background()
	id, _, err := container.Build(ctx, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("podman", "rmi", id).Run() })
	if v := AnsibleVersion(ctx, id); v != "9.9.9" {
		t.Errorf("version %q, want 9.9.9", v)
	}
	out, err := exec.Command("podman", "image", "inspect", "--format", "{{.ManifestType}}", id).Output()
	if err != nil || !strings.Contains(string(out), "oci") {
		t.Errorf("manifest type %q (%v), want OCI", out, err)
	}
}
