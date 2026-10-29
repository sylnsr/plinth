// Package image manages a namespace's runner image: published from the repo's default branch as Forgejo has it,
// never from an agent's checkout, pushed to the namespace's image repo in Forgejo's registry with the user's own podman
// login, and pinned by digest in the namespace's config.yml. plinthctl calls it; the controller never does: it only runs
// the pin. Registry calls go through skopeo, which reads podman's login; Plinth passes no credentials and never reads
// the login file. Spec: plinthctl.README.md, "The image"; the user's guide: docs/runner-image.md.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/forgejo"
	"plinth/internal/gitrepo"
	"plinth/internal/names"
)

// Labels a published image carries: the commit it was built from, the tree of its .plinth/, the Containerfile's
// sha256, the namespace (container.ImageLabel), and the ansible-core version it holds, or "none".
const (
	RevisionLabel = "org.opencontainers.image.revision"
	TreeLabel     = "plinth.tree"
	SumLabel      = "plinth.containerfile.sha256"
	AnsibleLabel  = "plinth.ansible-core"
)

// registryTimeout bounds a push or a pull; maxDiff the diff publish shows.
var (
	registryTimeout = 30 * time.Minute
	maxDiff         = 256 << 10
)

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	objectRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// Repo is the namespace's image repo, <host>/<owner>/plinth-<repo>, lowercased, and its registry host: forgejo-url's
// host and port, so there is no config key for it.
func Repo(forgejoURL string, ns names.Namespace) (repo, host string, err error) {
	u, err := url.Parse(forgejoURL)
	if err != nil || u.Host == "" {
		return "", "", errors.New("forgejo-url has no host") // never quote it
	}
	host = strings.ToLower(u.Host)
	return host + "/" + strings.ToLower(ns.Owner) + "/plinth-" + strings.ToLower(ns.Repo), host, nil
}

// Tag is a published image's tag: the commit's first 12 hex digits and the UTC time, unique per publish, so a rebuild
// of the same commit doesn't untag the earlier image. The tag is for people; the pin is the digest.
func Tag(commit string, t time.Time) string {
	return commit[:12] + "-" + t.UTC().Format("20060102T150405Z")
}

// LoggedIn checks that the user is logged in to the registry at host (podman login --get-login), naming the command
// that logs in when not.
func LoggedIn(host string, ns names.Namespace) error {
	if LoginUser(host) == "" {
		return fmt.Errorf("not logged in to %s: run plinthctl image login -n %s", host, ns)
	}
	return nil
}

// LoginUser is the user logged in to the registry at host (podman login --get-login), "" for none.
func LoginUser(host string) string {
	out, err := exec.Command("podman", "login", "--get-login", host).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Login runs podman login for host in the user's terminal: the password goes to podman, never to Plinth.
func Login(host string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("podman", "login", host)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// Logout runs podman logout for host: the user's login goes.
func Logout(host string, stdout, stderr io.Writer) error {
	cmd := exec.Command("podman", "logout", host)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Plan is a publish up to the user's confirmation: the branch fetched from Forgejo into <ns-dir>/state/git/, the
// commit, its .plinth/ and what Plinth's checks found in it, and the diff since the pin. Close removes the repo.
type Plan struct {
	NS         names.Namespace
	Repo       string // the image repo
	Host       string // its registry
	Branch     string // the default branch, as Forgejo names it
	Protection forgejo.Branch
	Commit     string
	Tree       string // the tree of .plinth/ at Commit
	Sum        string // the Containerfile's sha256
	Files      []gitrepo.File
	Findings   []container.Finding
	DiffFrom   string // what Diff is against: the pinned commit, or "" for every file in full
	DiffNote   string // why the diff shows every file in full, when there is a pin
	Diff       string
	DiffCut    bool // Diff stops at maxDiff
	git        string
	lock       *os.File
}

// Prepare fetches the default branch from Forgejo (never the agent's checkout) into <ns-dir>/state/git/, a new bare
// repo, and reads .plinth/ at commit ("" for the branch's head; a commit must be on the branch), checks it and diffs
// it against the pin's commit (every file in full on the first publish, or when the pinned commit isn't on the branch
// any more). One publish per namespace at a time: a lock in <ns-dir>/state/. A repo a crash left is removed first.
func Prepare(ctx context.Context, fj *forgejo.Client, nsDir string, ns names.Namespace, pin config.ImagePin, commit string) (p *Plan, err error) {
	if commit != "" && !objectRe.MatchString(commit) {
		return nil, fmt.Errorf("%q is not a full commit id", commit)
	}
	repo, host, err := Repo(fj.URL, ns)
	if err != nil {
		return nil, err
	}
	p = &Plan{NS: ns, Repo: repo, Host: host, git: filepath.Join(nsDir, "state", "git")}
	if p.lock, err = lock(nsDir); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			p.Close()
			p = nil
		}
	}()
	if p.Branch, err = fj.DefaultBranch(ctx, ns); err != nil {
		return p, err
	}
	if p.Protection, err = fj.Branch(ctx, ns, p.Branch); err != nil {
		return p, err
	}
	if err := os.RemoveAll(p.git); err != nil {
		return p, err
	}
	head, err := gitrepo.Fetch(p.git, gitrepo.Remote{URL: fj.RepoURL(ns), Token: fj.Token, CAFile: fj.CAFile}, p.Branch)
	if err != nil {
		return p, err
	}
	if commit == "" {
		commit = head
	} else if !gitrepo.OnBranch(p.git, p.Branch, commit) {
		return p, fmt.Errorf("commit %s is not on %s", commit, p.Branch)
	}
	if p.Commit, p.Tree, err = gitrepo.Tree(p.git, commit, container.RepoDir); err != nil {
		return p, fmt.Errorf("%s at %s: no %s directory", ns, commit, container.RepoDir)
	}
	if p.Files, err = gitrepo.Files(p.git, p.Tree, container.MaxContext); err != nil {
		return p, err
	}
	for _, f := range p.Files {
		if f.Path == container.Containerfile {
			s := sha256.Sum256(f.Data)
			p.Sum = hex.EncodeToString(s[:])
		}
	}
	p.Findings = append(container.Check(p.Files), baseFindings(ctx, p.Files)...)
	switch {
	case pin.Commit == "":
	case gitrepo.OnBranch(p.git, p.Branch, pin.Commit):
		p.DiffFrom = pin.Commit
	default:
		p.DiffNote = "the pinned commit " + pin.Commit + " is not on " + p.Branch + " any more"
	}
	if p.Diff, p.DiffCut, err = gitrepo.Diff(p.git, p.DiffFrom, p.Commit, container.RepoDir, maxDiff); err != nil {
		return p, err
	}
	return p, nil
}

// lock takes <ns-dir>/state/image.lock, or says another image command holds it.
func lock(nsDir string) (*os.File, error) {
	if _, err := os.Stat(nsDir); err != nil {
		return nil, fmt.Errorf("%s: no such namespace directory (plinthctl register sets it up)", nsDir)
	}
	if err := os.MkdirAll(filepath.Join(nsDir, "state"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(nsDir, "state", "image.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another image publish of this namespace is running")
	}
	return f, nil
}

// Close removes <ns-dir>/state/git/ and releases the lock; a retry fetches again (a lasting copy would keep history
// that a force-push removed from Forgejo, a secret say).
func (p *Plan) Close() error {
	if p == nil {
		return nil
	}
	err := os.RemoveAll(p.git)
	if p.lock != nil {
		p.lock.Close()
		p.lock = nil
	}
	return err
}

// baseFindings refuses each base image whose config has ONBUILD triggers: the build runs them, where
// Check never sees them, and podman keeps them from an image in Docker format. A base this machine has is inspected
// here, as the build will use it; any other in its registry, with the user's login. A config that can't be read refuses
// the build too.
func baseFindings(ctx context.Context, files []gitrepo.File) []container.Finding {
	var out []container.Finding
	for _, f := range files {
		if f.Path != container.Containerfile {
			continue
		}
		for _, b := range container.Bases(f.Data) {
			refuse := func(format string, a ...any) {
				out = append(out, container.Finding{File: f.Path, Line: b.Line, Refused: true, Text: fmt.Sprintf(format, a...)})
			}
			switch triggers, err := onBuild(ctx, b); {
			case err != nil:
				refuse("FROM %s: its config couldn't be read to look for ONBUILD triggers: %v", b.Ref, err)
			case len(triggers) > 0:
				t := strings.Join(triggers, "; ")
				if len(t) > 500 {
					t = t[:500] + "..."
				}
				refuse("FROM %s has ONBUILD triggers, which the build would run unchecked: %s", b.Ref, t)
			}
		}
	}
	return out
}

// onBuild is the ONBUILD triggers in the config of the base image b, for b's platform: the config as stored (skopeo
// inspect --config --raw); without --raw, skopeo gives it converted to OCI, which has no OnBuild.
func onBuild(ctx context.Context, b container.Base) ([]string, error) {
	var args []string
	if b.Platform != "" { // skopeo's own options, before the command
		p := strings.Split(b.Platform, "/")
		args = append(args, "--override-os", p[0])
		if len(p) > 1 {
			args = append(args, "--override-arch", p[1])
		}
		if len(p) > 2 {
			args = append(args, "--override-variant", p[2])
		}
	}
	src := "docker://" + b.Ref
	if Present(b.Ref) {
		src = "containers-storage:" + b.Ref
	}
	out, err := skopeoOut(ctx, append(args, "inspect", "--config", "--raw", src)...)
	if err != nil {
		return nil, err
	}
	var c struct {
		Config struct{ OnBuild []string } `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return nil, fmt.Errorf("its config: %v", err)
	}
	return c.Config.OnBuild, nil
}

// Refused is the findings that stop the build.
func (p *Plan) Refused() []container.Finding { return container.Refused(p.Findings) }

// Published is what Publish made: the tag, the digest the registry gave the pushed image, the pin, and the
// ansible-core version in the image.
type Published struct {
	Tag     string
	Digest  string
	Pin     config.ImagePin
	Ansible string // the ansible-core version in the image (AnsibleLabel), or "none"
}

// Publish builds the plan's .plinth/ (progress gets podman build's output, control characters taken out), pushes it to
// the image repo tagged Tag(commit, now) with the user's login, calls pin with the pin (the caller writes it into the
// namespace's config.yml), then pulls the image back by its digest, so this machine has exactly the pinned bits (a push
// can re-encode layers). An error after pin was called leaves the pin in place, and says to pull it.
func (p *Plan) Publish(ctx context.Context, now time.Time, progress io.Writer, pin func(config.ImagePin) error) (Published, error) {
	if refused := p.Refused(); len(refused) > 0 {
		return Published{}, fmt.Errorf("refused: %s", refused[0])
	}
	pub := Published{Tag: Tag(p.Commit, now)}
	if progress == nil {
		progress = io.Discard
	}
	base, _, err := container.Build(ctx, p.Files, Clean(progress),
		"--label", RevisionLabel+"="+p.Commit, "--label", TreeLabel+"="+p.Tree, "--label", SumLabel+"="+p.Sum,
		"--label", container.ImageLabel+"="+p.NS.String())
	if err != nil {
		return pub, err
	}
	pub.Ansible = AnsibleVersion(ctx, base)
	fmt.Fprintf(progress, "ansible-core in the image: %s\n", pub.Ansible)
	id, err := addLabel(ctx, base, AnsibleLabel, pub.Ansible, p.Repo+":"+pub.Tag)
	// The build without the label: untagged, so it goes once its labelled child does (below), or now.
	defer exec.Command("podman", "rmi", base).Run()
	if err != nil {
		return pub, err
	}
	if pub.Digest, err = push(ctx, id, p.Repo+":"+pub.Tag); err != nil {
		return pub, err
	}
	pub.Pin = config.ImagePin{Ref: p.Repo + "@" + pub.Digest, Commit: p.Commit, Tree: p.Tree}
	if err := pin(pub.Pin); err != nil {
		return pub, fmt.Errorf("pushed %s:%s (%s), but the pin wasn't written: %w", p.Repo, pub.Tag, pub.Digest, err)
	}
	if err := Pull(ctx, p.Repo, pub.Digest, pub.Tag); err != nil {
		return pub, fmt.Errorf("pinned %s, but pulling it back failed (plinthctl image pull -n %s): %w", pub.Pin.Ref, p.NS, err)
	}
	// The local build, when the pulled image is another one: untagged now, so it goes.
	if pulled, err := imageID(pub.Pin.Ref); err == nil && pulled != id {
		exec.Command("podman", "rmi", id).Run()
	}
	return pub, nil
}

// ansibleRe is the version in `ansible --version`'s first line: "ansible [core 2.19.11]".
var ansibleRe = regexp.MustCompile(`(?m)^ansible \[core ([0-9][0-9A-Za-z.+~-]{0,62})\]`)

// AnsibleVersion is the ansible-core version the image id holds, from `ansible --version` run in it as contained as a
// run (no network, a read-only root, no capability), or "none" when it has no ansible or doesn't answer within a
// minute. ansible is the entrypoint, so an image's own ENTRYPOINT doesn't get --version as its arguments (devops
// review of a6e9ad6). A distro build can carry fixes its version number doesn't show, so the version is shown, never judged.
func AnsibleVersion(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--env=HOME=/tmp", "--entrypoint=ansible", id, "--version")
	cmd.WaitDelay = time.Second
	out, _ := cmd.Output()
	if m := ansibleRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return "none"
}

// addLabel builds the image id with the label name=value added, tagged tag, and returns the new image's ID: a build of
// FROM id and LABEL alone, which runs nothing: Check refuses ONBUILD, and id is in OCI format, which has none. value is "none" or a version ansibleRe matched: nothing to quote.
func addLabel(ctx context.Context, id, name, value, tag string) (string, error) {
	dir, err := os.MkdirTemp("", "plinth-label-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cf, iid := filepath.Join(dir, "Containerfile"), filepath.Join(dir, "iid")
	if err := os.WriteFile(cf, []byte("FROM "+id+"\nLABEL "+name+"="+value+"\n"), 0o600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var msg bytes.Buffer
	cmd := exec.CommandContext(ctx, "podman", "build", "--format", "oci", "--pull=never", "--iidfile", iid, "--tag", tag, "--file", cf, dir)
	w := Clean(&msg)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman build (the %s label): %v: %s", name, err, strings.TrimSpace(msg.String()))
	}
	b, err := os.ReadFile(iid)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "sha256:"), nil
}

// push copies the local image id to dest (<repo>:<tag>) with skopeo and returns the digest the registry has for it.
func push(ctx context.Context, id, dest string) (string, error) {
	dir, err := os.MkdirTemp("", "plinth-push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	df := filepath.Join(dir, "digest")
	if err := skopeo(ctx, "copy", "--quiet", "--digestfile", df, "containers-storage:"+id, "docker://"+dest); err != nil {
		return "", err
	}
	b, err := os.ReadFile(df)
	if d := strings.TrimSpace(string(b)); err == nil && digestRe.MatchString(d) {
		return d, nil
	}
	return "", fmt.Errorf("skopeo copy to %s: no digest", dest)
}

// Pull copies <repo>@<digest> from the registry to this machine, always by digest, so a moved tag pulls nothing
// else; locally it is named <repo>:<tag>, or <repo>@<digest> without a tag. Afterwards podman must find it by that
// digest, which is what the controller runs (--pull=never).
func Pull(ctx context.Context, repo, digest, tag string) error {
	if !digestRe.MatchString(digest) {
		return fmt.Errorf("%q is not sha256:<64 hex>", digest)
	}
	dest := repo + "@" + digest
	if tag != "" {
		if !tagRe.MatchString(tag) {
			return fmt.Errorf("%q is not a tag", tag)
		}
		dest = repo + ":" + tag
	}
	if err := skopeo(ctx, "copy", "--quiet", "docker://"+repo+"@"+digest, "containers-storage:"+dest); err != nil {
		return err
	}
	if err := exec.Command("podman", "image", "exists", repo+"@"+digest).Run(); err != nil {
		return fmt.Errorf("pulled %s, but podman doesn't find it by that digest", repo+"@"+digest)
	}
	return nil
}

// Resolve is the digest a tag of repo names now, as the registry says (skopeo inspect).
func Resolve(ctx context.Context, repo, tag string) (string, error) {
	if !tagRe.MatchString(tag) {
		return "", fmt.Errorf("%q is not a tag", tag)
	}
	out, err := skopeoOut(ctx, "inspect", "--no-tags", "--format", "{{.Digest}}", "docker://"+repo+":"+tag)
	if d := strings.TrimSpace(out); err == nil && digestRe.MatchString(d) {
		return d, nil
	} else if err == nil {
		err = fmt.Errorf("skopeo inspect %s:%s: no digest", repo, tag)
	}
	return "", err
}

// imageID is the local ID of ref.
func imageID(ref string) (string, error) {
	out, err := exec.Command("podman", "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "sha256:"), nil
}

func skopeo(ctx context.Context, args ...string) error {
	_, err := skopeoOut(ctx, args...)
	return err
}

// skopeoOut runs skopeo with the user's own registry login (podman's), passing no credentials.
func skopeoOut(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "skopeo", args...)
	var out, msg bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &msg
	if err := cmd.Run(); err != nil {
		m := strings.TrimSpace(msg.String())
		if len(m) > 2048 {
			m = m[len(m)-2048:]
		}
		return "", fmt.Errorf("skopeo %s: %v: %s", args[0], err, m)
	}
	return out.String(), nil
}

// Clean writes to w what is written to it, each control character but newline and tab shown as ?: the text comes from
// the repo (a build's output, a diff), and the terminal shouldn't take orders from it.
func Clean(w io.Writer) io.Writer { return cleaner{w} }

type cleaner struct{ w io.Writer }

func (c cleaner) Write(p []byte) (int, error) {
	s := strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || r == unicode.ReplacementChar) {
			return '?'
		}
		return r
	}, string(p))
	if _, err := io.WriteString(c.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}
