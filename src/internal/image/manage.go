package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/names"
)

// Ref is an image as image use, pull and delete name it: a tag of the namespace's image repo, a digest in it
// (sha256:<hex>), or an image published elsewhere (<name>@sha256:<hex>; use only).
type Ref struct {
	Name   string // the image's repo
	Tag    string // "" when named by digest
	Digest string // sha256:<hex>; "" for a tag until it is resolved
}

// String is <name>@<digest>, or <name>:<tag> before the tag is resolved.
func (r Ref) String() string {
	if r.Digest != "" {
		return r.Name + "@" + r.Digest
	}
	return r.Name + ":" + r.Tag
}

// Host is the registry the image is in: its name's first part.
func (r Ref) Host() string {
	h, _, _ := strings.Cut(r.Name, "/")
	return h
}

// ParseRef reads arg against the namespace's image repo; elsewhere allows <name>@sha256:<hex>.
func ParseRef(arg, repo string, elsewhere bool) (Ref, error) {
	switch name, d, ok := strings.Cut(arg, "@"); {
	case ok && elsewhere:
		if !digestRe.MatchString(d) || !config.ValidImageRef(arg) {
			return Ref{}, fmt.Errorf("%q is not <name>@sha256:<64 hex>", arg)
		}
		return Ref{Name: name, Digest: d}, nil
	case ok:
		return Ref{}, fmt.Errorf("%q: name an image of %s by its tag or sha256:<digest>", arg, repo)
	case strings.HasPrefix(arg, "sha256:"):
		if !digestRe.MatchString(arg) {
			return Ref{}, fmt.Errorf("%q is not sha256:<64 hex>", arg)
		}
		return Ref{Name: repo, Digest: arg}, nil
	case tagRe.MatchString(arg):
		return Ref{Name: repo, Tag: arg}, nil
	}
	return Ref{}, fmt.Errorf("%q is not a tag or sha256:<digest>", arg)
}

// Info is what the registry says about an image (skopeo inspect).
type Info struct {
	Digest  string
	Created time.Time
	Labels  map[string]string
}

// Inspect asks the registry about ref (<name>:<tag> or <name>@<digest>).
func Inspect(ctx context.Context, ref string) (Info, error) {
	out, err := skopeoOut(ctx, "inspect", "--no-tags", "docker://"+ref)
	if err != nil {
		return Info{}, err
	}
	var v struct {
		Digest  string
		Created time.Time
		Labels  map[string]string
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return Info{}, fmt.Errorf("skopeo inspect %s: %v", ref, err)
	}
	if !digestRe.MatchString(v.Digest) {
		return Info{}, fmt.Errorf("skopeo inspect %s: no digest", ref)
	}
	return Info{Digest: v.Digest, Created: v.Created, Labels: v.Labels}, nil
}

// PinOf is the pin for the image <name>@<info.Digest> once its labels say it is a Plinth image of ns: plinth.image
// names ns, and the revision and the tree are git ids. The pin's commit and tree are the labels'.
func PinOf(info Info, name string, ns names.Namespace) (config.ImagePin, error) {
	if got := info.Labels[container.ImageLabel]; got != ns.String() {
		return config.ImagePin{}, fmt.Errorf("not a Plinth image of %s (its %s label says %q)", ns, container.ImageLabel, got)
	}
	pin := config.ImagePin{Ref: name + "@" + info.Digest, Commit: info.Labels[RevisionLabel], Tree: info.Labels[TreeLabel]}
	if err := pin.Check(); err != nil {
		return config.ImagePin{}, fmt.Errorf("not a Plinth image of %s: %v", ns, err)
	}
	return pin, nil
}

// Tags lists the tags of repo (skopeo list-tags).
func Tags(ctx context.Context, repo string) ([]string, error) {
	out, err := skopeoOut(ctx, "list-tags", "docker://"+repo)
	if err != nil {
		return nil, err
	}
	var v struct{ Tags []string }
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("skopeo list-tags %s: %v", repo, err)
	}
	var tags []string
	for _, t := range v.Tags {
		if tagRe.MatchString(t) { // the registry's text: nothing else reaches the terminal
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// Delete deletes <repo>@<digest> from the registry, every tag on it with it. It can't be undone.
func Delete(ctx context.Context, repo, digest string) error {
	if !digestRe.MatchString(digest) {
		return fmt.Errorf("%q is not sha256:<64 hex>", digest)
	}
	return skopeo(ctx, "delete", "docker://"+repo+"@"+digest)
}

// Present reports whether this machine has ref (podman image exists).
func Present(ref string) bool {
	return exec.Command("podman", "image", "exists", ref).Run() == nil
}

// CheckLocal checks the image a run of ns gets: the pin, present on this machine (the controller never pulls), with the
// pin's labels: plinth.image names ns, the revision is the pin's commit and plinth.tree its tree.
func CheckLocal(pin config.ImagePin, ns names.Namespace) error {
	if pin.Ref == "" {
		return fmt.Errorf("no image pinned: plinthctl image publish -n %s", ns)
	}
	if err := pin.Check(); err != nil {
		return fmt.Errorf("plinth.exec.image: %v", err)
	}
	if !Present(pin.Ref) {
		return fmt.Errorf("image not pulled: run plinthctl image pull -n %s", ns)
	}
	out, err := exec.Command("podman", "image", "inspect", pin.Ref).Output()
	if err != nil {
		return fmt.Errorf("podman image inspect %s: %v", pin.Ref, err)
	}
	var v []struct{ Labels map[string]string }
	if err := json.Unmarshal(out, &v); err != nil || len(v) != 1 {
		return fmt.Errorf("podman image inspect %s: not one image", pin.Ref)
	}
	for _, l := range []struct{ name, want string }{
		{container.ImageLabel, ns.String()}, {RevisionLabel, pin.Commit}, {TreeLabel, pin.Tree},
	} {
		if got := v[0].Labels[l.name]; got != l.want {
			return fmt.Errorf("image %s doesn't match the pin: its label %s is %q, the pin says %q", pin.Ref, l.name, got, l.want)
		}
	}
	return nil
}

// SameLocal reports whether a and b name the same image on this machine (podman's image ID): a tag and the pin may
// name one image by different names.
func SameLocal(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	x, err := imageID(a)
	if err != nil {
		return false
	}
	y, err := imageID(b)
	return err == nil && x != "" && x == y
}

// RemoveLocal removes this machine's copy of ref (podman rmi).
func RemoveLocal(ref string) error {
	if out, err := exec.Command("podman", "rmi", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("podman rmi %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}
