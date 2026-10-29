package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"plinth/internal/events"
	"plinth/internal/names"
	"plinth/internal/term"
)

// fakeImages records the commands the Image screen runs and answers with fixtures.
type fakeImages struct {
	skopeo bool
	user   string
	local  []string
	list   string
	ran    [][]string
}

func (f *fakeImages) Skopeo() bool { return f.skopeo }

func (f *fakeImages) Login(string) string { return f.user }

func (f *fakeImages) Present(ref string) bool { return slices.Contains(f.local, ref) }

func (f *fakeImages) List(names.Namespace) ([]byte, error) { return []byte(f.list), nil }

func (f *fakeImages) Run(args []string) (int, error) {
	f.ran = append(f.ran, args)
	return 0, nil
}

var (
	pinDigest = "sha256:" + strings.Repeat("a", 64)
	pinRef    = "forgejo.example.org/michael/plinth-devops@" + pinDigest
)

// settle applies the results of background work (the podman checks, image list) until the Image screen has them all.
func settle(t *testing.T, m *Model) {
	t.Helper()
	for m.img != nil && (m.img.checking || m.img.loading) {
		select {
		case f := <-m.Async():
			f()
		case <-time.After(5 * time.Second):
			t.Fatal("no result of the Image screen's background work")
		}
	}
}

// TestImageScreen: I opens the namespace's image screen: the repo, the login, the pin with whether this machine has it
// and the last run's drift check, the protection note and the registry's images, every value sanitized. Each key runs
// exactly its plinthctl image command with the selected digest; a dimmed key runs nothing; without skopeo only l runs.
func TestImageScreen(t *testing.T) {
	m, work := setup(t)
	write(t, filepath.Join(work, "michael", "devops", "config.yml"), "plinth:\n  exec:\n    image:\n      ref: "+pinRef+
		"\n      commit: "+strings.Repeat("c", 40)+"\n      tree: "+strings.Repeat("e", 40)+"\n")
	if err := os.MkdirAll(filepath.Join(work, "michael", "devops", "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	evlog, err := events.Open(work, filepath.Join("michael", "devops"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evlog.Append(events.Event{Caller: "worker", Action: "exec.start", Target: "main/calm-otter", Outcome: "ok", Rule: "allow",
		Reason: "ask.sh sha256 x; mode normal; image " + pinRef + "; commit " + strings.Repeat("c", 40) +
			"; drift: main at " + strings.Repeat("d", 40) + " has .plinth tree " + strings.Repeat("f", 40) + ", the pin has " +
			strings.Repeat("e", 40) + "; run directory: ask.sh"}); err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("b", 64)
	third := "sha256:" + strings.Repeat("9", 64)
	f := &fakeImages{skopeo: true, user: "tester", list: `repo: forgejo.example.org/michael/plinth-devops
pin: ` + pinRef + `
protection: {branch: main, protected: false, required-approvals: 0}
images:
  - {tag: cccccccccccc-20261017T080000Z, digest: "` + pinDigest + `", commit: ` + strings.Repeat("c", 40) + `, tree: ` + strings.Repeat("e", 40) + `, created: 2026-10-17T08:00:00Z, plinth: true, pinned: true, local: true}
  - {tag: "x\u001b]0;forged\u0007", digest: "` + other + `", created: 2026-10-16T08:00:00Z, plinth: false, pinned: false, local: false}
  - {tag: dddddddddddd-20261015T080000Z, digest: "` + third + `", commit: ` + strings.Repeat("d", 40) + `, created: 2026-10-15T08:00:00Z, plinth: true, pinned: false, local: false}
`}
	m.store.Images = f
	m.Reload()
	runes(m, "I")
	if s := frame(t, m); !strings.Contains(s, "loading") || !strings.Contains(s, "Login         checking") {
		t.Errorf("before the background work:\n%s", s)
	}
	settle(t, m)
	s := frame(t, m)
	for _, want := range []string{"Image michael/devops", "forgejo.example.org/michael/plinth-devops", "logged in to forgejo.example.org as tester",
		"Pinned        sha256:aaaaaaaaaaaa  not on this machine: p pulls it",
		"tag cccccccccccc-20261017T080000Z  commit cccccccccccc  tree eeeeeeeeeeee",
		".plinth/ changed since the image was published (run main/calm-otter, ",
		"main at dddddddddddd has .plinth tree ffffffffffff, the pin has eeeeeeeeeeee", "Branch protection is recommended: see docs/runner-image.md",
		"pinned", "not Plinth", "x\\x1b]0;forged\\x07"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in\n%s", want, s)
		}
	}

	// The header's hints and the breadcrumb.
	hints := "<l> Login  <o> Logout  <b> Publish  <u> Use  <p> Pull  <ctrl-d> Delete  <shift-r> Reload"
	layout(t, m, 80, 24, hints, "<:> Screens  <?> Help", "<all> <runs> <image>")
	layout(t, m, 120, 40, hints, "<:> Screens  <?> Help", "<all> <runs> <image>")

	base := func(sub string, rest ...string) []string {
		return append([]string{"image", sub}, rest...)
	}
	nsArgs := []string{"-n", "michael/devops", "--work", work}
	// While the list loads again (R), no row shows or is selected: no key acts on an image that isn't on the screen.
	runes(m, "R")
	runes(m, "u")
	if len(f.ran) != 0 || !strings.Contains(frame(t, m), "Use: no image selected") {
		t.Errorf("use while loading ran %v\n%s", f.ran, frame(t, m))
	}
	settle(t, m)
	if !strings.Contains(frame(t, m), "list reloaded (3 images)") {
		t.Errorf("after R:\n%s", frame(t, m))
	}

	want := func(what string, args ...string) {
		t.Helper()
		if len(f.ran) == 0 || !slices.Equal(f.ran[len(f.ran)-1], args) {
			t.Errorf("%s ran %v, want %v", what, f.ran, args)
		}
		settle(t, m) // the list again after every command
	}
	// On the pin: no use, no delete; p pulls it.
	runes(m, "u")
	ctrl(m, 'd')
	if len(f.ran) != 0 || !strings.Contains(frame(t, m), "Delete: pinned") {
		t.Errorf("use or delete of the pin ran %v\n%s", f.ran, frame(t, m))
	}
	runes(m, "p")
	want("p", append(append(base("pull"), nsArgs...), pinDigest)...)
	// Not a Plinth image: no use; delete asks which, and any other key cancels.
	runes(m, "j")
	runes(m, "u")
	if len(f.ran) != 1 || !strings.Contains(frame(t, m), "not a Plinth image") {
		t.Errorf("use of a foreign image ran %v", f.ran)
	}
	ctrl(m, 'd')
	runes(m, "x")
	if len(f.ran) != 1 {
		t.Errorf("a cancelled delete ran %v", f.ran)
	}
	ctrl(m, 'd')
	runes(m, "1")
	want("delete --local", append(append(base("delete", "--local"), nsArgs...), other)...)
	runes(m, "j")
	ctrl(m, 'd')
	runes(m, "2")
	want("delete --registry", append(append(base("delete", "--registry"), nsArgs...), third)...)
	runes(m, "u")
	want("u", append(append(base("use"), nsArgs...), third)...)
	runes(m, "b")
	want("b", append(base("publish"), nsArgs...)...)
	runes(m, "l")
	want("l", append(base("login"), nsArgs...)...)
	if !strings.Contains(frame(t, m), "plinthctl image login: done.") {
		t.Errorf("message:\n%s", frame(t, m))
	}
	runes(m, "o")
	want("o", append(base("logout"), nsArgs...)...)
	f.user = ""
	runes(m, "R")
	settle(t, m)
	runes(m, "o")
	if !strings.Contains(frame(t, m), "Log out: not logged in") || len(f.ran) != 7 {
		t.Errorf("logout when not logged in ran %v\n%s", f.ran, frame(t, m))
	}
	f.user = "tester"

	// Without skopeo: only l; no list.
	f.skopeo, f.ran = false, nil
	runes(m, "R")
	settle(t, m)
	lists := len(m.Async())
	runes(m, "b")
	runes(m, "p")
	if len(f.ran) != 0 || len(m.Async()) != lists || !strings.Contains(frame(t, m), "Pull: needs skopeo (package skopeo)") {
		t.Errorf("without skopeo ran %v\n%s", f.ran, frame(t, m))
	}
	runes(m, "l")
	if len(f.ran) != 1 {
		t.Errorf("l without skopeo ran %v", f.ran)
	}
	settle(t, m)
	key(m, term.KeyEsc)
	if m.top().kind != screenRuns || m.img != nil {
		t.Error("Esc didn't leave the Image screen")
	}
}
