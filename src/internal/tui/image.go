package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"plinth/internal/config"
	"plinth/internal/events"
	"plinth/internal/image"
	"plinth/internal/names"
	"plinth/internal/runner"
	"plinth/internal/term"
)

// Images is what the Image screen runs: podman's local checks, and plinthctl's image commands as child
// processes, so the TUI itself still opens no network connection. Tests replace it.
type Images interface {
	Skopeo() bool                            // skopeo is installed: the image commands need it
	Login(host string) string                // the user logged in to the registry (podman login --get-login), "" for none
	Present(ref string) bool                 // this machine has the image (podman image exists)
	List(ns names.Namespace) ([]byte, error) // plinthctl image list -o yaml, in the background: its stdout
	Run(args []string) (int, error)          // plinthctl <args> in the terminal, handed over: its exit status
}

// listTimeout bounds image list: skopeo inspects every image in the registry. podmanWait bounds each of the header's
// podman checks, which run in the background too: a podman that hangs (its storage lock held by a build or a pull)
// leaves the header "checking" for that long, never the screen frozen. waitDelay is how long a command's output is
// waited for once its bound has passed.
const (
	listTimeout = 5 * time.Minute
	podmanWait  = 10 * time.Second
	waitDelay   = time.Second
)

// hostImages is Images on this machine: plinthctl is self, Run hands the terminal over through handover.
type hostImages struct {
	work     string
	self     string
	handover func(*exec.Cmd) (int, error)
}

func (h *hostImages) Skopeo() bool {
	_, err := exec.LookPath("skopeo")
	return err == nil
}

// bounded is name args with ctx as its bound: once ctx is done, the command is killed, and its output is no longer
// waited for after waitDelay, even if a child it started (rootless podman's user-namespace re-exec) holds it.
func bounded(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = waitDelay
	return cmd
}

func (h *hostImages) Login(host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), podmanWait)
	defer cancel()
	out, err := bounded(ctx, "podman", "login", "--get-login", host).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (h *hostImages) Present(ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), podmanWait)
	defer cancel()
	return bounded(ctx, "podman", "image", "exists", ref).Run() == nil
}

func (h *hostImages) List(ns names.Namespace) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := bounded(ctx, h.self, "image", "list", "-o", "yaml", "-n", ns.String(), "--work", h.work)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if l := lastLine(stderr.String()); l != "" {
			return nil, errors.New(l)
		}
		return nil, fmt.Errorf("plinthctl image list: %v", err)
	}
	return out, nil
}

func (h *hostImages) Run(args []string) (int, error) {
	return h.handover(exec.Command(h.self, args...))
}

// lastLine is the last line of s that isn't empty.
func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

// imageList is what plinthctl image list -o yaml prints.
type imageList struct {
	Repo       string `yaml:"repo"`
	Pin        string `yaml:"pin"`
	Protection struct {
		Branch            string `yaml:"branch"`
		Protected         bool   `yaml:"protected"`
		RequiredApprovals int    `yaml:"required-approvals"`
		Error             string `yaml:"error"`
	} `yaml:"protection"`
	Images []imageRow `yaml:"images"`
}

type imageRow struct {
	Tag     string    `yaml:"tag"`
	Digest  string    `yaml:"digest"`
	Commit  string    `yaml:"commit"`
	Tree    string    `yaml:"tree"`
	Created time.Time `yaml:"created"`
	Plinth  bool      `yaml:"plinth"`
	Pinned  bool      `yaml:"pinned"`
	Local   bool      `yaml:"local"`
}

// imageScreen is the Image screen's state: its header is read when it opens, on R and after every command; its list
// comes from image list, in the background.
type imageScreen struct {
	ns       names.Namespace
	repo     string
	host     string
	err      error // forgejo-url has no host
	skopeo   bool
	user     string // logged in to the registry as; "" for not logged in
	pin      string // the pin the next three are about
	pinLocal bool
	check    string // the drift check of the last run of the pin (exec.start), "" for no run yet
	checkRun string
	checkAt  string
	checking bool // the login and pinLocal are being read
	loading  bool // the list is being read; no row shows or is selected meanwhile
	announce bool // say on the message line when the list is back (R)
	seq      int  // of the load under way: a later load or leaving the screen drops an earlier one
	list     *imageList
	listErr  string
}

// openImage opens the Image screen for ri's namespace, or the filtered one.
func (m *Model) openImage(ri *runInfo) {
	sp := m.space()
	if ri != nil {
		sp = ri.sp
	}
	if sp == nil {
		m.say(normal, "choose a namespace first (n)")
		return
	}
	if m.store.Images == nil {
		m.say(errStyle, "the Image screen is not available here")
		return
	}
	m.img = &imageScreen{ns: sp.ns}
	m.push(&view{kind: screenImage, crumb: "image", title: "Image " + sp.ns.String()})
	m.loadImage()
}

// loadImage reads the header again and starts, in the background, its podman checks (the login, whether this machine
// has the pin) and image list; each hands its result to the loop (Async). Until the list is back no row shows, so no
// key acts on an image that isn't on the screen.
func (m *Model) loadImage() {
	im := m.img
	im.repo, im.host, im.err = image.Repo(m.snap.g.ForgejoURL, im.ns)
	im.skopeo = m.store.Images.Skopeo()
	im.user, im.pinLocal, im.check, im.checkRun, im.checkAt = "", false, "", "", ""
	if im.pin = m.pinOf(im.ns).Ref; im.pin != "" {
		im.check, im.checkRun, im.checkAt = m.store.lastCheck(im.ns, im.pin)
	}
	im.seq++
	im.checking, im.list, im.listErr = true, nil, ""
	im.loading = im.skopeo && im.err == nil
	seq, images, ns, host, pin, list := im.seq, m.store.Images, im.ns, im.host, im.pin, im.loading
	current := func() bool { return m.img == im && im.seq == seq }
	go func() {
		user, local := "", false
		if host != "" {
			user = images.Login(host)
		}
		if pin != "" {
			local = images.Present(pin)
		}
		m.async <- func() {
			if current() {
				im.checking, im.user, im.pinLocal = false, user, local
			}
		}
		if !list {
			return
		}
		var l imageList
		out, err := images.List(ns)
		if err == nil {
			if uerr := yaml.Unmarshal(out, &l); uerr != nil {
				err = fmt.Errorf("image list: %v", uerr)
			}
		}
		m.async <- func() {
			if !current() {
				return
			}
			im.loading = false
			if err != nil {
				im.listErr = err.Error()
			} else {
				im.list = &l
				if im.announce {
					m.say(normal, fmt.Sprintf("list reloaded (%d images)", len(l.Images)))
				}
			}
			im.announce = false
		}
	}()
}

// pinOf is the namespace's pin (plinth.exec.image) in the snapshot; the zero pin for none.
func (m *Model) pinOf(ns names.Namespace) config.ImagePin {
	for _, sp := range m.snap.spaces {
		if sp.ns == ns && sp.cfg != nil {
			return sp.cfg.Image
		}
	}
	return config.ImagePin{}
}

// imageAt is the image the cursor is on; nil when the list is empty or not loaded.
func (m *Model) imageAt(v *view) *imageRow {
	if m.img == nil || m.img.loading || m.img.list == nil || v.cursor < 0 || v.cursor >= len(m.img.list.Images) {
		return nil
	}
	return &m.img.list.Images[v.cursor]
}

// imageActs are the Image screen's keys; one that can't act now has its note, said when pressed.
func (m *Model) imageActs(v *view) []act {
	im, row := m.img, m.imageAt(v)
	needs := ""
	if !im.skopeo {
		needs = "needs skopeo (package skopeo)"
	}
	b := act{key: "b", label: "Publish", note: needs}
	u := act{key: "u", label: "Use", note: needs}
	p := act{key: "p", label: "Pull", note: needs}
	d := act{key: "ctrl-d", label: "Delete", note: needs}
	switch {
	case needs != "":
	case row == nil:
		u.note, d.note = "no image selected", "no image selected"
		p.label = "Pull the pin"
		if m.pinOf(im.ns).Ref == "" {
			p.note = "no image pinned"
		}
	case row.Pinned:
		u.note, d.note = "pinned", "pinned"
	case !row.Plinth:
		u.note = "not a Plinth image of " + im.ns.String()
	}
	o := act{key: "o", label: "Log out"}
	if !im.checking && im.user == "" {
		o.note = "not logged in"
	}
	return []act{{key: "l", label: "Log in"}, o, b, u, p, d}
}

func (m *Model) imageKey(v *view, k term.Key) {
	if m.screenKey(k) {
		return
	}
	name := keyName(k)
	switch {
	case k.Code == term.KeyEsc || (k.Code == term.KeyRune && k.Rune == 'q'):
		m.pop()
		return
	case k.Code == term.KeyRune && k.Rune == 'R':
		if m.img.loading || m.img.checking {
			m.say(normal, "loading the list")
			return
		}
		m.loadImage()
		m.img.announce = m.img.loading
		return
	case k.Code != term.KeyRune && name != "ctrl-d":
		return
	}
	var a *act
	for _, x := range m.imageActs(v) {
		if x.key == name {
			a = &x
			break
		}
	}
	if a == nil {
		return
	}
	if a.note != "" {
		m.say(dimStyle, a.label+": "+a.note)
		return
	}
	im, row := m.img, m.imageAt(v)
	digest := func() []string {
		if row == nil {
			return nil
		}
		return []string{row.Digest}
	}
	switch name {
	case "l":
		m.imageCommand("login", nil)
	case "o":
		m.imageCommand("logout", nil)
	case "b":
		m.imageCommand("publish", nil)
	case "u":
		m.imageCommand("use", nil, row.Digest)
	case "p":
		m.imageCommand("pull", nil, digest()...)
	case "ctrl-d":
		box := []*term.Line{
			text("Delete ", normal).Untrusted(row.Tag, boldStyle).Untrusted(" ("+row.Digest+") of "+im.ns.String()+":", normal),
			text("  1  this machine's copy (podman rmi)", normal),
			text("  2  from the registry, for every machine: it can't be undone (you type yes)", normal),
		}
		m.choose(box, text("1 or 2? Any other key cancels.", normal), map[rune]func(){
			'1': func() { m.imageCommand("delete", []string{"--local"}, row.Digest) },
			'2': func() { m.imageCommand("delete", []string{"--registry"}, row.Digest) },
		})
	}
}

// imageCommand runs plinthctl image <sub> for the screen's namespace in the terminal, handed over: the command asks its
// own questions. Then the screen reads everything again, and the message line shows the exit status.
func (m *Model) imageCommand(sub string, flags []string, pos ...string) {
	im := m.img
	args := append(append([]string{"image", sub}, flags...), "-n", im.ns.String(), "--work", m.store.Work)
	args = append(args, pos...)
	what := "plinthctl " + strings.Join(append(append([]string{"image", sub}, flags...), pos...), " ")
	code, err := m.store.Images.Run(args)
	lerr := m.store.log("ran a command", "command", what, "ns", im.ns, "exit", code)
	m.Reload()
	if m.img == im {
		m.loadImage()
	}
	switch {
	case err != nil && code < 0:
		m.say(errStyle, what+": "+err.Error())
	case code != 0:
		m.say(errStyle, fmt.Sprintf("%s: exit %d", what, code))
	case lerr != nil:
		m.say(errStyle, what+": done. Not in the general log: "+lerr.Error())
	default:
		m.say(normal, what+": done.")
	}
}

// lastCheck is the drift check of the last run of the image ref, as its exec.start says: the check, the run and
// when; all "" while no run used the image.
func (s *Store) lastCheck(ns names.Namespace, ref string) (check, run, at string) {
	events.Walk(s.Work, filepath.Join(ns.Owner, ns.Repo), func(e events.Event, _ []byte) error {
		if e.Action != "exec.start" {
			return nil
		}
		if c, ok := runner.PinCheck(e.Reason, ref); ok {
			check, run, at = c, e.Target, e.Time
		}
		return nil
	})
	return check, run, at
}

func (m *Model) imageView(v *view, w, rows int) (title, body []*term.Line) {
	im := m.img
	title = []*term.Line{text("Image ", headStyle).Untrusted(im.ns.String(), headStyle)}
	field := func(name string, value *term.Line) {
		body = append(body, text(fmt.Sprintf("%-14s", name), boldStyle).Append(value))
	}
	if im.err != nil {
		field("Image repo", untrusted(im.err.Error(), errStyle))
	} else {
		field("Image repo", untrusted(im.repo, normal))
	}
	switch {
	case im.err != nil:
	case im.checking:
		field("Login", text("checking", dimStyle))
	case im.user == "":
		field("Login", untrusted("not logged in to "+im.host+": press l", errStyle))
	default:
		field("Login", untrusted("logged in to "+im.host+" as "+im.user, normal))
	}
	pin := m.pinOf(im.ns)
	if pin.Ref == "" {
		field("Pinned", text("No image pinned: b publishes one, u uses one from the list", errStyle))
	} else {
		l := text("sha256:", normal).Untrusted(short(strings.TrimPrefix(pin.Digest(), "sha256:")), normal)
		switch {
		case im.pin != pin.Ref:
		case im.checking:
			l.Text("  checking", dimStyle)
		case im.pinLocal:
			l.Text("  here", normal)
		default:
			l.Text("  not on this machine: p pulls it", errStyle)
		}
		field("Pinned", l)
		l = &term.Line{}
		for _, r := range m.imageRows() {
			if r.Pinned {
				l.Text("tag ", normal).Untrusted(r.Tag, normal).Text("  ", normal)
			}
		}
		field("", l.Text("commit ", normal).Untrusted(short(pin.Commit), normal).Text("  tree ", normal).Untrusted(short(pin.Tree), normal))
		if im.err == nil && pin.Name() != im.repo {
			field("", text("published elsewhere: ", normal).Untrusted(pin.Name(), normal))
		}
		run := " (run " + im.checkRun + ", " + im.checkAt + ")"
		switch c := im.check; {
		case im.pin != pin.Ref:
		case c == "":
			field("Drift", text("not checked yet: the controller checks before each run", dimStyle))
		case strings.HasPrefix(c, "drift: "):
			field("Drift", untrusted(".plinth/ changed since the image was published"+run+": publish again to run it", errStyle))
			field("", untrusted(hashRe.ReplaceAllStringFunc(strings.TrimPrefix(c, "drift: "), short), errStyle))
		default:
			field("Drift", untrusted(c+run, normal))
		}
	}
	if l := im.list; l != nil {
		switch p := l.Protection; {
		case p.Error != "":
			body = append(body, untrusted("Branch protection: unknown ("+p.Error+")", dimStyle))
		case !p.Protected || p.RequiredApprovals == 0:
			body = append(body, text("Branch protection is recommended: see docs/runner-image.md", errStyle))
		}
	}
	if !im.skopeo {
		body = append(body, text("The image commands need skopeo (package skopeo): only l works without it.", errStyle))
	}
	body = append(body, nil)
	cols := []int{29, 12, 12, 7, 5, max(1, w-70)}
	var heads []*term.Line
	for _, s := range []string{"TAG", "DIGEST", "COMMIT", "CREATED", "LOCAL", ""} {
		heads = append(heads, text(s, boldStyle))
	}
	body = append(body, row(cols, heads, normal))
	switch {
	case im.loading:
		return title, append(body, text("loading", dimStyle))
	case im.listErr != "":
		return title, append(body, untrusted(im.listErr, errStyle))
	case im.list == nil:
		return title, body
	case len(im.list.Images) == 0:
		return title, append(body, text("No images.", dimStyle))
	}
	imgs := im.list.Images
	v.cursor = max(0, min(v.cursor, len(imgs)-1))
	n := max(1, rows-len(body))
	scroll(v, v.cursor, n)
	for i := v.top; i < len(imgs) && i < v.top+n; i++ {
		r := imgs[i]
		st := actStyle
		if i == v.cursor {
			st = selStyle
		}
		local, mark := "no", ""
		if r.Local {
			local = "yes"
		}
		switch {
		case r.Pinned:
			mark = "pinned"
		case !r.Plinth:
			mark = "not Plinth" // its labels don't name this namespace; u says so in full. 10 columns at 80
		}
		cells := []*term.Line{untrusted(r.Tag, st), untrusted(short(strings.TrimPrefix(r.Digest, "sha256:")), st),
			untrusted(or(short(r.Commit), "-"), st), text(age(m.store.Now(), r.Created), st), text(local, st), untrusted(mark, st)}
		body = append(body, row(cols, cells, st).PadTo(w, st))
	}
	return title, body
}

// imageRows is the loaded list; nil while there is none.
func (m *Model) imageRows() []imageRow {
	if m.img == nil || m.img.loading || m.img.list == nil {
		return nil
	}
	return m.img.list.Images
}

// hashRe is a git or sha256 hash in a drift check's text: shown by its first 12 characters.
var hashRe = regexp.MustCompile(`[0-9a-f]{40,64}`)

// short is the first 12 characters of a hash.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
