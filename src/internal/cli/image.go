package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"plinth/internal/config"
	"plinth/internal/forgejo"
	"plinth/internal/image"
	"plinth/internal/logfile"
	"plinth/internal/names"
)

// imageArgs is -n, --work and up to maxPos args, with both configs loaded: every image command needs the namespace's
// image repo (from forgejo-url) and its pin.
type imageArgs struct {
	work  string
	ns    names.Namespace
	pos   []string
	g     *config.Global
	cfg   *config.Namespace
	repo  string // the namespace's image repo
	host  string // its registry
	nsDir string
}

// more adds a command's own flags (nil for none).
func parseImageArgs(c *command, args []string, stdout, stderr io.Writer, maxPos int, more func(*flag.FlagSet)) (*imageArgs, int, bool) {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	n := flags.String("n", "", "the namespace <owner>/<repo> (default: the origin of the checkout you are in)")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	if more != nil {
		more(flags)
	}
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return nil, code, false
	}
	if len(pos) > maxPos {
		return nil, usageError(c, stderr, "too many arguments"), false
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return nil, exitFail, false
	}
	ns, code, ok := namespaceArg(c, *n, w, stderr)
	if !ok {
		return nil, code, false
	}
	a := &imageArgs{work: w, ns: ns, pos: pos, nsDir: filepath.Join(w, ns.Owner, ns.Repo)}
	if a.g, err = config.LoadGlobal(w, os.LookupEnv); err == nil {
		a.cfg, _, err = config.LoadNamespace(w, ns, a.g, os.LookupEnv)
	}
	if err == nil {
		a.repo, a.host, err = image.Repo(a.g.ForgejoURL, ns)
	}
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return nil, exitFail, false
	}
	return a, exitOK, true
}

// signalContext ends with Ctrl-C or SIGTERM: a build or a push stops, and the command cleans up.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runImageLogin(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 0, nil)
	if !ok {
		return code
	}
	fmt.Fprintf(stdout, "podman login %s: the password goes to podman, never to Plinth.\n", a.host)
	if err := image.Login(a.host, stdin, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "plinthctl image login: %v\n", err)
		return exitFail
	}
	return exitOK
}

// runImageLogout logs the user out of the namespace's registry (podman logout): the counterpart of image login.
func runImageLogout(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 0, nil)
	if !ok {
		return code
	}
	if err := image.Logout(a.host, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "plinthctl image logout: %v\n", err)
		return exitFail
	}
	return exitOK
}

// runImagePublish fetches the default branch from Forgejo, shows what Plinth's checks found and the diff since the
// pin, and after the user types publish builds, pushes with the user's login, pins and pulls back.
func runImagePublish(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 1, nil)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl image publish: %v\n", err)
		return exitFail
	}
	commit := ""
	if len(a.pos) == 1 {
		commit = a.pos[0]
	}
	if err := image.LoggedIn(a.host, a.ns); err != nil {
		return fail(err)
	}
	fj, err := forgejo.New(a.g.ForgejoURL, a.g.ForgejoToken, "")
	if err != nil {
		return fail(err)
	}
	ctx, stop := signalContext()
	defer stop()
	fmt.Fprintf(stdout, "Fetching the default branch of %s from Forgejo ...\n", a.ns)
	plan, err := image.Prepare(ctx, fj, a.nsDir, a.ns, a.cfg.Image, commit)
	if err != nil {
		return fail(err)
	}
	defer plan.Close()
	showPlan(stdout, plan, a.cfg.Image)
	if len(plan.Refused()) > 0 {
		fmt.Fprintln(stdout, "Refused: nothing is built. Fix .plinth/ on the branch and publish again.")
		return exitFail
	}
	if !confirm(stdout, "Build "+a.repo+" from this, push it and pin it in "+a.ns.String()) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return exitFail
	}
	log := logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns)
	pub, err := plan.Publish(ctx, time.Now(), stdout, func(pin config.ImagePin) error {
		if err := config.SetImage(a.work, a.ns, a.g, os.LookupEnv, pin); err != nil {
			return err
		}
		log.Info("image published", "ref", pin.Ref, "commit", pin.Commit, "tree", pin.Tree)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	log.Info("image holds ansible-core", "ref", pub.Pin.Ref, "ansible", pub.Ansible)
	fmt.Fprintf(stdout, "Published %s:%s (%s, ansible-core %s) and pinned it in %s's config.yml; this machine has it.\n",
		a.repo, pub.Tag, pub.Digest, pub.Ansible, a.ns)
	return exitOK
}

// showPlan prints what the user confirms: where the image comes from, the branch protection note, the diff since the
// pin, and every finding.
func showPlan(w io.Writer, p *image.Plan, pin config.ImagePin) {
	fmt.Fprintf(w, "Image repo:  %s\nBranch:      %s at %s\n.plinth/:    tree %s\n", p.Repo, p.Branch, p.Commit, p.Tree)
	if pin != (config.ImagePin{}) {
		fmt.Fprintf(w, "Pinned now:  %s (commit %s)\n", pin.Ref, pin.Commit)
		if pin.Tree == p.Tree {
			fmt.Fprintln(w, "The pinned image was built from this same .plinth/ tree.")
		}
	}
	if !p.Protection.Protected || p.Protection.RequiredApprovals == 0 {
		fmt.Fprintln(w, "Branch protection is recommended: see docs/runner-image.md.")
	}
	fmt.Fprintln(w)
	switch {
	case p.DiffFrom != "":
		fmt.Fprintf(w, "Changes in .plinth/ since the pinned commit %s:\n", p.DiffFrom)
	case p.DiffNote != "":
		fmt.Fprintf(w, "Every file in .plinth/ (%s):\n", p.DiffNote)
	default:
		fmt.Fprintln(w, "Every file in .plinth/ (no image pinned yet):")
	}
	if p.Diff == "" {
		fmt.Fprintln(w, "  (none)")
	}
	io.WriteString(image.Clean(w), p.Diff)
	if p.DiffCut {
		fmt.Fprintln(w, "... (the diff goes on: read the rest on Forgejo before you publish)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Findings (Plinth's checks decide):")
	if len(p.Findings) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, f := range p.Findings {
		fmt.Fprintf(w, "  %s\n", f)
	}
	fmt.Fprintln(w, "  Ollama review: not available")
}

func runImagePull(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 1, nil)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl image pull: %v\n", err)
		return exitFail
	}
	ctx, stop := signalContext()
	defer stop()
	name, host, digest, tag := a.repo, a.host, "", ""
	switch {
	case len(a.pos) == 0:
		if a.cfg.Image == (config.ImagePin{}) {
			return fail(fmt.Errorf("%s has no pinned image: plinthctl image publish -n %s", a.ns, a.ns))
		}
		name, digest = a.cfg.Image.Name(), a.cfg.Image.Digest()
		host, _, _ = strings.Cut(name, "/")
	case strings.HasPrefix(a.pos[0], "sha256:"):
		digest = a.pos[0]
	default:
		tag = a.pos[0]
	}
	if err := image.LoggedIn(host, a.ns); err != nil {
		return fail(err)
	}
	if tag != "" {
		var err error
		if digest, err = image.Resolve(ctx, name, tag); err != nil {
			return fail(err)
		}
	}
	if err := image.Pull(ctx, name, digest, tag); err != nil {
		return fail(err)
	}
	logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns).Info("image pulled", "name", name, "digest", digest)
	fmt.Fprintf(stdout, "Pulled %s@%s.\n", name, digest)
	return exitOK
}

// runImageUse pins an image the registry has, a Plinth image of this namespace by its labels, after the user types use.
func runImageUse(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 1, nil)
	if !ok {
		return code
	}
	if len(a.pos) != 1 {
		return usageError(c, stderr, "one <tag>, sha256:<digest> or <name>@sha256:<digest> is required")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl image use: %v\n", err)
		return exitFail
	}
	ref, err := image.ParseRef(a.pos[0], a.repo, true)
	if err != nil {
		return usageError(c, stderr, err.Error())
	}
	if err := image.LoggedIn(ref.Host(), a.ns); err != nil {
		return fail(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if ref.Digest == "" {
		if ref.Digest, err = image.Resolve(ctx, ref.Name, ref.Tag); err != nil {
			return fail(err)
		}
	}
	info, err := image.Inspect(ctx, ref.String())
	if err != nil {
		return fail(err)
	}
	pin, err := image.PinOf(info, ref.Name, a.ns)
	if err != nil {
		return fail(err)
	}
	cur := a.cfg.Image
	if pin == cur {
		fmt.Fprintf(stdout, "%s is the pin of %s already.\n", pin.Ref, a.ns)
		return exitOK
	}
	fmt.Fprintf(stdout, "Image:    %s\n", pin.Ref)
	if ref.Tag != "" {
		fmt.Fprintf(stdout, "Tag:      %s (resolved now: the pin is the digest)\n", ref.Tag)
	}
	fmt.Fprintf(stdout, "Commit:   %s\nTree:     %s\nCreated:  %s\n", pin.Commit, pin.Tree, info.Created.UTC().Format(time.RFC3339))
	if ref.Name != a.repo {
		fmt.Fprintf(stdout, "Published elsewhere, not in %s.\n", a.repo)
	}
	switch {
	case cur == (config.ImagePin{}):
		fmt.Fprintln(stdout, "No image is pinned yet.")
	case cur.Tree == pin.Tree:
		fmt.Fprintf(stdout, "Pinned now: %s, built from the same .plinth/ tree.\n", cur.Ref)
	default:
		fmt.Fprintf(stdout, "Pinned now: %s (commit %s, tree %s): another .plinth/ tree, so what runs get changes.\n",
			cur.Ref, cur.Commit, cur.Tree)
	}
	if !confirm(stdout, "Pin "+pin.Ref+" in "+a.ns.String()) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return exitFail
	}
	if err := config.SetImage(a.work, a.ns, a.g, os.LookupEnv, pin); err != nil {
		return fail(err)
	}
	logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns).Info("image pinned", "ref", pin.Ref, "commit", pin.Commit, "tree", pin.Tree, "was", cur.Ref)
	fmt.Fprintf(stdout, "Pinned %s in %s's config.yml.\n", pin.Ref, a.ns)
	if !image.Present(pin.Ref) {
		fmt.Fprintf(stdout, "This machine doesn't have it yet: plinthctl image pull -n %s\n", a.ns)
	}
	return exitOK
}

// listed is one image of the namespace's image repo, as image list shows it.
type listed struct {
	Tag     string    `yaml:"tag"`
	Digest  string    `yaml:"digest"`
	Commit  string    `yaml:"commit,omitempty"`
	Tree    string    `yaml:"tree,omitempty"`
	Created time.Time `yaml:"created"`
	Plinth  bool      `yaml:"plinth"` // its labels name this namespace
	Pinned  bool      `yaml:"pinned"`
	Local   bool      `yaml:"local"` // this machine has it
}

// protection is the default branch's protection, as far as Plinth's read token sees it.
type protection struct {
	Branch            string `yaml:"branch,omitempty"`
	Protected         bool   `yaml:"protected"`
	RequiredApprovals int    `yaml:"required-approvals"`
	Error             string `yaml:"error,omitempty"`
}

// runImageList lists the images of the namespace's image repo, newest first, with the pin and this machine's copies
// marked, and the default branch's protection (for the TUI's note).
func runImageList(c *command, args []string, stdout, stderr io.Writer) int {
	var format string
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 0, func(f *flag.FlagSet) {
		f.StringVar(&format, "o", "", "output format: yaml")
	})
	if !ok {
		return code
	}
	if format != "" && format != "yaml" {
		return usageError(c, stderr, "-o takes yaml only")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl image list: %v\n", err)
		return exitFail
	}
	if err := image.LoggedIn(a.host, a.ns); err != nil {
		return fail(err)
	}
	ctx, stop := signalContext()
	defer stop()
	tags, err := image.Tags(ctx, a.repo)
	if err != nil {
		return fail(fmt.Errorf("%w (nothing published yet?)", err))
	}
	var rows []listed
	for _, tag := range tags {
		info, err := image.Inspect(ctx, a.repo+":"+tag)
		if err != nil {
			fmt.Fprintf(stderr, "plinthctl image list: %s: %v\n", tag, err)
			continue
		}
		row := listed{Tag: tag, Digest: info.Digest, Created: info.Created.UTC(), Pinned: a.repo+"@"+info.Digest == a.cfg.Image.Ref,
			Local: image.Present(a.repo + "@" + info.Digest)}
		if pin, err := image.PinOf(info, a.repo, a.ns); err == nil {
			row.Plinth, row.Commit, row.Tree = true, pin.Commit, pin.Tree
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(x, y listed) int { return y.Created.Compare(x.Created) })
	prot := branchProtection(ctx, a)
	if format == "yaml" {
		out, err := yaml.Marshal(struct {
			Repo       string     `yaml:"repo"`
			Pin        string     `yaml:"pin"`
			Protection protection `yaml:"protection"`
			Images     []listed   `yaml:"images"`
		}{a.repo, a.cfg.Image.Ref, prot, rows})
		if err != nil {
			return fail(err)
		}
		stdout.Write(out)
		return exitOK
	}
	fmt.Fprintf(stdout, "Image repo: %s\n", a.repo)
	switch pin := a.cfg.Image; {
	case pin == (config.ImagePin{}):
		fmt.Fprintln(stdout, "Pinned:     none (plinthctl image publish or image use sets it)")
	case pin.Name() != a.repo:
		fmt.Fprintf(stdout, "Pinned:     %s (published elsewhere)\n", pin.Ref)
	default:
		fmt.Fprintf(stdout, "Pinned:     %s\n", pin.Ref)
	}
	switch {
	case prot.Error != "":
		fmt.Fprintf(stdout, "Branch protection: unknown (%s)\n", prot.Error)
	case !prot.Protected || prot.RequiredApprovals == 0:
		fmt.Fprintf(stdout, "Branch protection: off on %s - recommended: see docs/runner-image.md\n", prot.Branch)
	default:
		fmt.Fprintf(stdout, "Branch protection: on for %s (%d approval(s) required)\n", prot.Branch, prot.RequiredApprovals)
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No images.")
		return exitOK
	}
	fmt.Fprintf(stdout, "\n%-30s %-19s %-12s %-20s %s\n", "TAG", "DIGEST", "COMMIT", "CREATED", "")
	for _, r := range rows {
		var marks []string
		if r.Pinned {
			marks = append(marks, "pinned")
		}
		if r.Local {
			marks = append(marks, "here")
		}
		if !r.Plinth {
			marks = append(marks, "not a Plinth image of "+a.ns.String())
		}
		commit := r.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Fprintf(stdout, "%-30s %-19s %-12s %-20s %s\n", r.Tag, r.Digest[:19], commit, r.Created.Format(time.RFC3339), strings.Join(marks, ", "))
	}
	return exitOK
}

// branchProtection asks Forgejo for the default branch's protection; an error goes into the result, since the list
// is still worth showing without it.
func branchProtection(ctx context.Context, a *imageArgs) protection {
	fj, err := forgejo.New(a.g.ForgejoURL, a.g.ForgejoToken, "")
	if err != nil {
		return protection{Error: err.Error()}
	}
	branch, err := fj.DefaultBranch(ctx, a.ns)
	if err != nil {
		return protection{Error: err.Error()}
	}
	b, err := fj.Branch(ctx, a.ns, branch)
	if err != nil {
		return protection{Branch: branch, Error: err.Error()}
	}
	return protection{Branch: branch, Protected: b.Protected, RequiredApprovals: b.RequiredApprovals}
}

// runImageDelete removes this machine's copy of an image (--local), or deletes it from the registry (--registry,
// after the user types delete); never the pinned image.
func runImageDelete(c *command, args []string, stdout, stderr io.Writer) int {
	var local, registry bool
	a, code, ok := parseImageArgs(c, args, stdout, stderr, 1, func(f *flag.FlagSet) {
		f.BoolVar(&local, "local", false, "remove this machine's copy")
		f.BoolVar(&registry, "registry", false, "delete it from the registry")
	})
	if !ok {
		return code
	}
	if local == registry {
		return usageError(c, stderr, "one of --local and --registry is required")
	}
	if len(a.pos) != 1 {
		return usageError(c, stderr, "one <tag> or sha256:<digest> is required")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl image delete: %v\n", err)
		return exitFail
	}
	ref, err := image.ParseRef(a.pos[0], a.repo, false)
	if err != nil {
		return usageError(c, stderr, err.Error())
	}
	pinned := func(digest string) bool { return a.cfg.Image.Ref == a.repo+"@"+digest }
	log := logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns)
	if local {
		if !image.Present(ref.String()) {
			return fail(fmt.Errorf("this machine has no %s", ref))
		}
		if (ref.Digest != "" && pinned(ref.Digest)) || image.SameLocal(ref.String(), a.cfg.Image.Ref) {
			return fail(fmt.Errorf("%s is pinned: use another image first", ref))
		}
		if err := image.RemoveLocal(ref.String()); err != nil {
			return fail(err)
		}
		log.Info("image: removed this machine's copy", "ref", ref)
		fmt.Fprintf(stdout, "Removed this machine's copy of %s.\n", ref)
		return exitOK
	}
	if err := image.LoggedIn(a.host, a.ns); err != nil {
		return fail(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if ref.Digest == "" {
		if ref.Digest, err = image.Resolve(ctx, ref.Name, ref.Tag); err != nil {
			return fail(err)
		}
	}
	if pinned(ref.Digest) {
		return fail(fmt.Errorf("%s is pinned: use another image first", ref))
	}
	fmt.Fprintf(stdout, "This deletes %s from the registry, with every tag on it. It can't be undone.\n", ref)
	fmt.Fprintln(stdout, "Another machine may pin it: its next image pull then fails (Plinth checks only this machine's pin).")
	if !confirm(stdout, "Delete "+ref.String()+" from the registry") {
		fmt.Fprintln(stdout, "Nothing changed.")
		return exitFail
	}
	if err := image.Delete(ctx, ref.Name, ref.Digest); err != nil {
		return fail(err)
	}
	log.Info("image deleted from the registry", "ref", ref, "tag", a.pos[0])
	fmt.Fprintf(stdout, "Deleted %s from the registry.\n", ref)
	return exitOK
}
