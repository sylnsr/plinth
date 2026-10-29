// Package agents registers the agents of a namespace. Each register generates a new token, which the caller prints once
// and Plinth never stores: only its sha256 is kept, in <ns-dir>/state/agents/<agent>.sha256, next to <agent>.repo, the
// repo directory the agent registered from. A new register from the same repo replaces both, so the agent's previous
// token stops working; one from another repo waits beside them (PendingSuffix). A registered agent acts only once the
// user has enabled it for its repo in the namespace config (Enabled).
// Spec: plinthctl.README.md, "Agent tokens".
package agents

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"plinth/internal/names"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// TokenPrefix starts every agent token, so a DLP scan can mask one by a fixed pattern.
const TokenPrefix = "plinth_"

// tokenBytes is the token's randomness: 256 bits.
const tokenBytes = 32

// Dir is the directory below <ns-dir> that holds each agent's <agent>.sha256 and <agent>.repo.
const Dir = "state/agents"

// PendingSuffix names the slot of a registration from another repo than the agent's current one: <agent>.pending.sha256
// and .repo, waiting for the user's agent enable. Not an agent name (it has a dot), so List never counts it.
const PendingSuffix = ".pending"

// NewToken returns a new random token: TokenPrefix and 43 characters of base64url.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Fingerprint is the sha256 of token, in hex: the only form in which Plinth keeps a token.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Register sets up <work> and the namespace (package workdir; it only creates what is missing), records repo as the
// agent's repo directory, generates a new token and stores its sha256, replacing the agent's previous one. It returns
// the token and every path it created, relative to work. repo must be an absolute path.
func Register(work string, ns names.Namespace, agent, repo string) (string, []string, error) {
	if !names.ValidAgent(agent) {
		return "", nil, fmt.Errorf("agent %q: not a valid agent name ([a-z][a-z0-9-]{0,31})", agent)
	}
	if !filepath.IsAbs(repo) || strings.ContainsAny(repo, "\n\x00") {
		return "", nil, fmt.Errorf("repo %q: not an absolute path", repo)
	}
	created, err := workdir.EnsureWork(work)
	if err != nil {
		return "", created, err
	}
	c, err := workdir.EnsureNamespace(work, ns)
	created = append(created, c...)
	if err != nil {
		return "", created, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return "", created, err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	// A registration from another repo never replaces the agent's current one: it waits beside it, as
	// <agent>.pending.*, until the user enables it (Promote). So a register from a repo whose origin points at this
	// namespace by mistake, or by an agent's edit of .git/config, can't cut the real session off.
	slot := agent
	cur, err := Get(work, ns, agent)
	if err != nil {
		return "", created, err
	}
	if cur.Repo != "" && cur.Repo != repo {
		slot = agent + PendingSuffix
	}
	// The repo first: the sha256 is what makes the registration count, so it is written last.
	if err := safefs.Replace(root, dir, slot+".repo", []byte(repo+"\n"), 0o600); err != nil {
		return "", created, err
	}
	token, err := NewToken()
	if err != nil {
		return "", created, err
	}
	if err := safefs.Replace(root, dir, slot+".sha256", []byte(Fingerprint(token)+"\n"), 0o600); err != nil {
		return "", created, err
	}
	return token, created, nil
}

// Rotate generates a new token for the agent's current registration and stores its sha256, replacing the previous
// one, which stops working: register without its setup, and without moving the registration to another repo. A pending
// registration keeps its own token. It fails if the agent is not registered in the namespace.
func Rotate(work string, ns names.Namespace, agent string) (string, error) {
	cur, err := Get(work, ns, agent)
	if err != nil {
		return "", err
	}
	if cur.Repo == "" {
		return "", fmt.Errorf("agent %s is not registered in %s (plinthctl register registers it)", agent, ns)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return "", err
	}
	defer root.Close()
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	if err := safefs.Replace(root, dir, agent+".sha256", []byte(Fingerprint(token)+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// Remove deletes the agent's sha256 and repo files, and those of a pending registration, so its tokens stop working. It fails if the agent is not
// registered in the namespace.
func Remove(work string, ns names.Namespace, agent string) error {
	if !names.ValidAgent(agent) {
		return fmt.Errorf("agent %q: not a valid agent name", agent)
	}
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	removed := 0
	// The sha256 first: once it is gone the token no longer works, whatever happens to the repo file.
	for _, name := range []string{agent + ".sha256", agent + ".repo", agent + PendingSuffix + ".sha256", agent + PendingSuffix + ".repo"} {
		err := safefs.Remove(root, filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		removed++
	}
	if removed == 0 {
		return fmt.Errorf("agent %s is not registered in %s", agent, ns)
	}
	return nil
}

// List returns the agents registered in the namespace: every valid <agent> with a regular <agent>.sha256 (no symlink),
// sorted. A namespace without state/agents/ has none.
func List(work string, ns names.Namespace) ([]string, error) {
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	if _, err := safefs.Lstat(root, dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		agent, ok := strings.CutSuffix(e.Name(), ".sha256")
		if ok && e.Type().IsRegular() && names.ValidAgent(agent) {
			out = append(out, agent)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Registration is an agent's registration in a namespace, from its files.
type Registration struct {
	Repo    string // the repo top level of its current registration; "" when it has none (no <agent>.sha256)
	Pending string // the repo of a registration from another repo, waiting beside it for agent enable; "" when none
}

// Get reads the agent's registration.
func Get(work string, ns names.Namespace, agent string) (Registration, error) {
	var r Registration
	if !names.ValidAgent(agent) {
		return r, fmt.Errorf("agent %q: not a valid agent name", agent)
	}
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return r, err
	}
	root, err := os.OpenRoot(work)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	if r.Repo, err = slotRepo(root, dir, agent); err != nil {
		return r, err
	}
	r.Pending, err = slotRepo(root, dir, agent+PendingSuffix)
	return r, err
}

// slotRepo is the repo of the registration in slot (<slot>.sha256 and <slot>.repo), "" when there is none.
func slotRepo(root *os.Root, dir, slot string) (string, error) {
	if _, err := safefs.Lstat(root, filepath.Join(dir, slot+".sha256")); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	b, err := safefs.ReadFile(root, filepath.Join(dir, slot+".repo"), 4096)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// Promote makes the agent's pending registration its current one: its token works from now on (once enabled), the
// previous one's stops. The repo is moved first, so in between neither token matches what the config enables.
func Promote(work string, ns names.Namespace, agent string) error {
	r, err := Get(work, ns, agent)
	if err != nil {
		return err
	}
	if r.Pending == "" {
		return fmt.Errorf("agent %s has no pending registration in %s", agent, ns)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo, filepath.FromSlash(Dir))
	for _, ext := range []string{".repo", ".sha256"} {
		if err := safefs.Rename(root, filepath.Join(dir, agent+PendingSuffix+ext), filepath.Join(dir, agent+ext)); err != nil {
			return err
		}
	}
	return nil
}

// Enabled reports whether the agent may act in the namespace: it has a current registration, and the namespace config
// enables it for that registration's repo (enabled: plinth.agents.enabled). A registered agent that isn't enabled is
// refused by decide (target.valid).
func Enabled(work string, ns names.Namespace, agent string, enabled map[string]string) (bool, error) {
	want := enabled[agent]
	if want == "" {
		return false, nil
	}
	r, err := Get(work, ns, agent)
	return err == nil && r.Repo == want, err
}

// ByToken finds the agent whose current registration token is: of every namespace below work, the agent whose
// <agent>.sha256 is token's fingerprint (a pending registration's never matches: it waits for agent enable). Every
// fingerprint is compared, each in constant time, so the time taken says nothing about which one matched; ok is false
// when none does. Whether the agent is enabled is the caller's to check (Enabled).
func ByToken(work, token string) (ns names.Namespace, agent string, ok bool, err error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return ns, "", false, nil
	}
	want := Fingerprint(token)
	nss, err := workdir.Namespaces(work)
	if err != nil {
		return ns, "", false, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return ns, "", false, err
	}
	defer root.Close()
	for _, n := range nss {
		list, err := List(work, n)
		if err != nil {
			return names.Namespace{}, "", false, err
		}
		for _, a := range list {
			b, err := safefs.ReadFile(root, filepath.Join(n.Owner, n.Repo, filepath.FromSlash(Dir), a+".sha256"), 4096)
			if err != nil {
				continue // removed meanwhile, or not a regular file: no match
			}
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(b))), []byte(want)) == 1 && !ok {
				ns, agent, ok = n, a, true
			}
		}
	}
	return ns, agent, ok, nil
}
