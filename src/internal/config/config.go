// Package config loads Plinth's two config levels, <work>/config.yml (global) and <ns-dir>/config.yml (namespace).
// The loader is strict and fails closed: an unknown key, a wrong type, a value out of range, a review type that doesn't
// exist or a <...> placeholder left from the template is a problem, and every problem is reported at once. A namespace
// takes each namespace key it leaves out from the global config, which takes it from the built-in default.
// Spec: plinthctl.README.md, "Config".
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"plinth/internal/names"
	"plinth/internal/safefs"
)

// FileName is the config file's name at both levels.
const FileName = "config.yml"

// maxSize is the largest config file the loader reads.
const maxSize = 1 << 20

var (
	placeholderRe = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9 _.,-]*>`)
	varRe         = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	typeNameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	loginRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
)

// Global is <work>/config.yml after defaults, expansion and validation.
type Global struct {
	ForgejoURL   string          `yaml:"forgejo-url"`
	ForgejoToken string          `yaml:"forgejo-token"`
	Reviewers    GlobalReviewers `yaml:"reviewers"`
	Ollama       Ollama          `yaml:"ollama"`
	Plinth       GlobalPlinth    `yaml:"plinth"`

	// EnvRefs names every environment variable the config refers to (${VAR} and -env keys): the controller removes them
	// from an ask script's environment.
	EnvRefs []string `yaml:"-"`
	// Warnings are not problems: e.g. a ${VAR} that isn't set (it expanded to "").
	Warnings []string `yaml:"-"`
	// SHA256 is the sha256 (hex) of the file as read, for the event log's config field.
	SHA256 string `yaml:"-"`
}

// GlobalReviewers is reviewers: in the global config.
type GlobalReviewers struct {
	Type      map[string]ReviewType `yaml:"type"`
	Automatic []string              `yaml:"automatic"` // the default for every namespace
}

// ReviewType is one review type: its prompt is its URLs' contents joined in order, or else its definition.
type ReviewType struct {
	URLs       []string `yaml:"urls"`
	Definition string   `yaml:"definition"`
}

// Ollama is ollama:.
type Ollama struct {
	Host           string       `yaml:"host"`
	Models         OllamaModels `yaml:"models"`
	TimeoutSeconds int          `yaml:"timeout-seconds"`
	RetrySeconds   int          `yaml:"retry-seconds"`
	MaxParallel    int          `yaml:"max-parallel"`
}

// OllamaModels is ollama.models:.
type OllamaModels struct {
	Review string `yaml:"review"`
	DLP    string `yaml:"dlp"`
}

// GlobalPlinth is plinth: in the global config.
type GlobalPlinth struct {
	MCP         Listener       `yaml:"mcp"`
	WebUI       WebUI          `yaml:"web-ui"`
	TUI         TUI            `yaml:"tui"`
	Messages    GlobalMessages `yaml:"messages"`
	Runner      Runner         `yaml:"runner"`
	Exec        Exec           `yaml:"exec"`
	EventLogMax int            `yaml:"event-log-max"`
	Log         Log            `yaml:"log"`
	Notice      Notice         `yaml:"notice"`
	Preview     Preview        `yaml:"preview"`
	AAARawDir   string         `yaml:"aaa-raw-dir"`
	// Repo is <owner>/<repo> on forgejo-url: where Save and Report post, one issue per run. Without it there is
	// neither.
	Repo     string   `yaml:"repo"`
	Autosave Autosave `yaml:"autosave"`
	// FailedScans is the retired plinth.failed-scans (now plinth.repo): read only to refuse it, naming the new key.
	FailedScans *RetiredFailedScans `yaml:"failed-scans"`
}

// Autosave is plinth.autosave:. Runs saves a run by itself right after Done; Failures reports a run's first failed
// check by itself. Both default to false.
type Autosave struct {
	Failures bool `yaml:"failures"`
	Runs     bool `yaml:"runs"`
}

// RetiredFailedScans is the retired plinth.failed-scans:.
type RetiredFailedScans struct {
	Repo string `yaml:"repo"`
}

// Listener is an address and a port.
type Listener struct {
	Listen string `yaml:"listen"`
	Port   int    `yaml:"port"`
}

// WebUI is plinth.web-ui:.
type WebUI struct {
	Listen         string    `yaml:"listen"`
	Port           int       `yaml:"port"`
	PublicURL      string    `yaml:"public-url"`
	RefreshSeconds int       `yaml:"refresh-seconds"`
	Auth           WebUIAuth `yaml:"auth"`
}

// WebUIAuth is plinth.web-ui.auth:.
type WebUIAuth struct {
	ClientID     string   `yaml:"client-id"`
	Users        []string `yaml:"users"`
	SessionHours int      `yaml:"session-hours"`
}

// TUI is plinth.tui:.
type TUI struct {
	RefreshSeconds int `yaml:"refresh-seconds"`
}

// GlobalMessages is plinth.messages: in the global config; ExpirationMinutes and Ungated are namespace defaults.
type GlobalMessages struct {
	RefreshSeconds    int     `yaml:"refresh-seconds"`
	CatalogueURL      string  `yaml:"catalogue-url"`
	ExpirationMinutes int     `yaml:"expiration-minutes"`
	Ungated           Ungated `yaml:"ungated"`
}

// Ungated is plinth.messages.ungated:.
type Ungated struct {
	Agents   []string `yaml:"agents"`
	Messages []string `yaml:"messages"`
}

// Runner is plinth.runner:.
type Runner struct {
	PollSeconds        int `yaml:"poll-seconds"`
	MaxParallelExec    int `yaml:"max-parallel-exec"`
	ExecTimeoutMinutes int `yaml:"exec-timeout-minutes"`
}

// Exec is plinth.exec: in the global config, the defaults of every namespace.
type Exec struct {
	// ImageRef is the retired plinth.exec.image-ref (the build from the checkout, before the pin): still read, so a
	// config from the old template loads, but unused; set, it is a warning to remove it.
	ImageRef *string `yaml:"image-ref"`
	// EnvFile is a file name in the namespace's directory in <work>: its variables are a script's environment; "" for
	// none.
	EnvFile string `yaml:"env-file"`
	// Network and Privileged allow scripts that ask for network or privileged mode (NETWORK=1, PRIVILEGED=1).
	Network    bool `yaml:"network"`
	Privileged bool `yaml:"privileged"`
	// Memory is a run's memory limit, swap included, as podman takes it: <n>k, <n>m or <n>g (default 512m).
	Memory string `yaml:"memory"`
}

// Log is plinth.log:.
type Log struct {
	MaxAgeDays int `yaml:"max-age-days"`
}

// Preview is plinth.preview:. Limit is the ask_preview calls an agent may make per hour.
type Preview struct {
	Limit int `yaml:"limit"`
}

// Notice is plinth.notice:. Expires is the days a notice stays open after its last line, unless something clears it.
type Notice struct {
	Expires int `yaml:"expires"`
}

// Namespace is a namespace's effective config: each key from the namespace's config.yml, else from the global config.
type Namespace struct {
	Automatic         []string
	ExpirationMinutes int
	UngatedAgents     []string
	UngatedMessages   []string
	EnvFile           string // plinth.exec.env-file
	Network           bool   // plinth.exec.network
	Privileged        bool   // plinth.exec.privileged
	Memory            string // plinth.exec.memory
	// Image is the pin, plinth.exec.image (namespace only): zero when the namespace has none.
	Image ImagePin
	// Enabled maps each enabled agent to the repo top level its registration must come from (plinth.agents.enabled,
	// namespace only). An agent not in it, or registered from another repo, is registered but can't act.
	Enabled map[string]string
	// From says, per key, where its value came from: "namespace" or "global".
	From map[string]string
	// SHA256 is the sha256 (hex) of the namespace's config.yml as read; of nothing when there is none.
	SHA256 string
}

// nsFile is <ns-dir>/config.yml as written; a nil pointer is a key the namespace leaves out.
type nsFile struct {
	Reviewers nsReviewers `yaml:"reviewers"`
	Plinth    nsPlinth    `yaml:"plinth"`
}

type nsReviewers struct {
	Automatic *[]string `yaml:"automatic"`
}

type nsPlinth struct {
	Messages nsMessages `yaml:"messages"`
	Agents   nsAgents   `yaml:"agents"`
	Exec     nsExec     `yaml:"exec"`
}

type nsExec struct {
	ImageRef   *string  `yaml:"image-ref"` // retired: see Exec.ImageRef
	EnvFile    *string  `yaml:"env-file"`
	Network    *bool    `yaml:"network"`
	Privileged *bool    `yaml:"privileged"`
	Memory     *string  `yaml:"memory"`
	Image      ImagePin `yaml:"image"`
}

// ImagePin is plinth.exec.image, the namespace's pinned image: the only image its runs use, once the controller
// switches to it. Only image publish and image use write it.
type ImagePin struct {
	Ref    string `yaml:"ref"`    // <name>@sha256:<64 hex>: a digest, never a tag
	Commit string `yaml:"commit"` // the reviewed commit it was built from
	Tree   string `yaml:"tree"`   // the git hash of that commit's .plinth/
}

var (
	pinRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,254}@sha256:[0-9a-f]{64}$`)
	objectRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// Check says what is wrong with a pin; the zero pin (none) is fine.
func (p ImagePin) Check() error {
	switch {
	case p == (ImagePin{}):
		return nil
	case !ValidImageRef(p.Ref):
		return fmt.Errorf("ref %q is not <name>@sha256:<64 hex>", p.Ref)
	case !objectRe.MatchString(p.Commit):
		return fmt.Errorf("commit %q is not a git commit id", p.Commit)
	case !objectRe.MatchString(p.Tree):
		return fmt.Errorf("tree %q is not a git tree id", p.Tree)
	}
	return nil
}

// ValidImageRef reports whether ref is <name>@sha256:<64 hex>, an image by its digest.
func ValidImageRef(ref string) bool {
	return pinRefRe.MatchString(ref) && !strings.Contains(ref, "//")
}

// Name is the pin's image name, before the @.
func (p ImagePin) Name() string {
	n, _, _ := strings.Cut(p.Ref, "@")
	return n
}

// Digest is the pin's digest, sha256:<64 hex>.
func (p ImagePin) Digest() string {
	_, d, _ := strings.Cut(p.Ref, "@")
	return d
}

type nsAgents struct {
	Enabled map[string]string `yaml:"enabled"`
}

type nsMessages struct {
	ExpirationMinutes *int      `yaml:"expiration-minutes"`
	Ungated           nsUngated `yaml:"ungated"`
}

type nsUngated struct {
	Agents   *[]string `yaml:"agents"`
	Messages *[]string `yaml:"messages"`
}

// Error lists every problem found in one config file.
type Error struct {
	File     string
	Problems []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %d problem(s):\n  %s", e.File, len(e.Problems), strings.Join(e.Problems, "\n  "))
}

// Getenv looks up an environment variable; os.LookupEnv in production.
type Getenv func(string) (string, bool)

// Defaults is the global config before the file is read.
func Defaults(work string) Global {
	return Global{
		Reviewers: GlobalReviewers{Automatic: []string{}},
		Ollama: Ollama{
			TimeoutSeconds: 300,
			RetrySeconds:   10,
			MaxParallel:    1,
		},
		Plinth: GlobalPlinth{
			MCP:         Listener{Listen: "0.0.0.0", Port: 8791},
			WebUI:       WebUI{Port: 8790, RefreshSeconds: 5, Auth: WebUIAuth{SessionHours: 12}},
			TUI:         TUI{RefreshSeconds: 2},
			Messages:    GlobalMessages{RefreshSeconds: 5, ExpirationMinutes: 60, Ungated: Ungated{Agents: []string{}, Messages: []string{}}},
			Runner:      Runner{PollSeconds: 1, MaxParallelExec: 1, ExecTimeoutMinutes: 120},
			Exec:        Exec{Memory: "512m"},
			EventLogMax: 10000,
			Log:         Log{MaxAgeDays: 30},
			Notice:      Notice{Expires: 14},
			Preview:     Preview{Limit: 60},
			AAARawDir:   filepath.Join(work, "aaa-raw"),
		},
	}
}

// Exists reports whether dir/config.yml exists (as anything: Load refuses what isn't a regular file).
func Exists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, FileName))
	return err == nil
}

// LoadGlobal reads <work>/config.yml. A missing file is a problem: forgejo-url is required.
func LoadGlobal(work string, getenv Getenv) (*Global, error) {
	file := filepath.Join(work, FileName)
	g := Defaults(work)
	p := &parser{getenv: getenv}
	if err := p.load(work, &g); err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	if g.Plinth.AAARawDir == "" {
		g.Plinth.AAARawDir = filepath.Join(work, "aaa-raw")
	}
	p.validateGlobal(&g)
	if len(p.problems) > 0 {
		return nil, &Error{File: file, Problems: p.problems}
	}
	slices.Sort(p.refs)
	g.EnvRefs = slices.Compact(p.refs)
	if len(g.Reviewers.Automatic) == 0 {
		p.warnings = append(p.warnings, noAutomatic)
	}
	g.Warnings = p.warnings
	g.SHA256 = hex.EncodeToString(p.sum[:])
	return &g, nil
}

// Plain is what the plain-AAA commands read of <work>/config.yml: they need no other part of Plinth.
type Plain struct {
	AAARawDir    string
	Reviewers    GlobalReviewers
	Ollama       Ollama
	ForgejoURL   string // with ReviewersKey: the prompts of reviewers.type get forgejo-token only from this host
	ForgejoToken string
}

// Keys of the global config that the plain-AAA commands read (LoadPlain).
const (
	rawDirKey    = "plinth.aaa-raw-dir"
	OllamaKey    = "ollama"
	ReviewersKey = "reviewers"
)

// RawDir is plinth.aaa-raw-dir for the plain-AAA commands that need nothing else (scrubber): LoadPlain with that key.
func RawDir(work string, getenv Getenv) (string, error) {
	pl, err := LoadPlain(work, getenv)
	if err != nil {
		return "", err
	}
	return pl.AAARawDir, nil
}

// LoadPlain reads <work>/config.yml, if it exists, for the plain-AAA commands (scrubber, dlp-scan, review, ollama ...):
// plinth.aaa-raw-dir and the keys below each of keys (OllamaKey; ReviewersKey, which brings forgejo-url and forgejo-token,
// for the prompts). Only a problem of one of those, or a file that can't be read or parsed, is an error, so a config
// without forgejo-url or with unset ${VAR}s elsewhere doesn't stop them. Without the file every key has its default:
// <work>/aaa-raw, and Ollama not configured.
func LoadPlain(work string, getenv Getenv, keys ...string) (*Plain, error) {
	g := Defaults(work)
	pl := &Plain{AAARawDir: g.Plinth.AAARawDir, Reviewers: g.Reviewers, Ollama: g.Ollama}
	if !Exists(work) {
		return pl, nil
	}
	file := filepath.Join(work, FileName)
	p := &parser{getenv: getenv}
	if err := p.load(work, &g); err != nil {
		return nil, &Error{File: file, Problems: []string{err.Error()}}
	}
	if g.Plinth.AAARawDir == "" {
		g.Plinth.AAARawDir = filepath.Join(work, "aaa-raw")
	}
	if !filepath.IsAbs(g.Plinth.AAARawDir) {
		p.problem("%s: %q is not an absolute path", rawDirKey, g.Plinth.AAARawDir)
	}
	p.validateReviewers(g.Reviewers)
	p.validateOllama(g.Ollama)
	if g.ForgejoURL != "" {
		p.httpURL("forgejo-url", g.ForgejoURL)
	}
	var mine []string
	if slices.Contains(keys, ReviewersKey) {
		keys = append(slices.Clip(keys), "forgejo-url", "forgejo-token")
	}
	keys = append(slices.Clip(keys), rawDirKey)
	for _, pr := range p.problems {
		if strictLineRe.MatchString(pr) { // a strict-decode problem located couldn't place: it may be one of keys (fail closed)
			mine = append(mine, pr)
			continue
		}
		for _, k := range keys {
			if strings.HasPrefix(pr, k+":") || strings.HasPrefix(pr, k+".") || strings.HasPrefix(pr, k+"[") {
				mine = append(mine, pr)
				break
			}
		}
	}
	if len(mine) > 0 {
		return nil, &Error{File: file, Problems: mine}
	}
	pl.AAARawDir, pl.Reviewers, pl.Ollama = g.Plinth.AAARawDir, g.Reviewers, g.Ollama
	pl.ForgejoURL, pl.ForgejoToken = strings.TrimSuffix(g.ForgejoURL, "/"), g.ForgejoToken
	return pl, nil
}

// LoadNamespace reads <work>/<owner>/<repo>/config.yml (a missing file sets nothing) and returns the namespace's
// effective config on top of g.
func LoadNamespace(work string, ns names.Namespace, g *Global, getenv Getenv) (*Namespace, []string, error) {
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(work, ns.Owner, ns.Repo)
	file := filepath.Join(dir, FileName)
	var f nsFile
	p := &parser{getenv: getenv, sum: sha256.Sum256(nil)}
	if Exists(dir) {
		if err := p.load(dir, &f); err != nil {
			return nil, nil, &Error{File: file, Problems: []string{err.Error()}}
		}
	}
	n := &Namespace{
		Automatic:         g.Reviewers.Automatic,
		ExpirationMinutes: g.Plinth.Messages.ExpirationMinutes,
		UngatedAgents:     g.Plinth.Messages.Ungated.Agents,
		UngatedMessages:   g.Plinth.Messages.Ungated.Messages,
		EnvFile:           g.Plinth.Exec.EnvFile,
		Network:           g.Plinth.Exec.Network,
		Privileged:        g.Plinth.Exec.Privileged,
		Memory:            g.Plinth.Exec.Memory,
		From:              map[string]string{},
		SHA256:            hex.EncodeToString(p.sum[:]),
	}
	for _, k := range []string{"reviewers.automatic", "plinth.messages.expiration-minutes", "plinth.messages.ungated.agents", "plinth.messages.ungated.messages",
		"plinth.exec.env-file", "plinth.exec.network", "plinth.exec.privileged", "plinth.exec.memory"} {
		n.From[k] = "global"
	}
	if v := f.Reviewers.Automatic; v != nil {
		n.Automatic, n.From["reviewers.automatic"] = *v, "namespace"
	}
	if v := f.Plinth.Messages.ExpirationMinutes; v != nil {
		n.ExpirationMinutes, n.From["plinth.messages.expiration-minutes"] = *v, "namespace"
	}
	if v := f.Plinth.Messages.Ungated.Agents; v != nil {
		n.UngatedAgents, n.From["plinth.messages.ungated.agents"] = *v, "namespace"
	}
	if v := f.Plinth.Messages.Ungated.Messages; v != nil {
		n.UngatedMessages, n.From["plinth.messages.ungated.messages"] = *v, "namespace"
	}
	if f.Plinth.Exec.ImageRef != nil {
		p.warnings = append(p.warnings, retiredImageRef)
	}
	if v := f.Plinth.Exec.EnvFile; v != nil {
		n.EnvFile, n.From["plinth.exec.env-file"] = *v, "namespace"
	}
	if v := f.Plinth.Exec.Network; v != nil {
		n.Network, n.From["plinth.exec.network"] = *v, "namespace"
	}
	if v := f.Plinth.Exec.Privileged; v != nil {
		n.Privileged, n.From["plinth.exec.privileged"] = *v, "namespace"
	}
	if v := f.Plinth.Exec.Memory; v != nil {
		n.Memory, n.From["plinth.exec.memory"] = *v, "namespace"
	}
	n.Enabled, n.From["plinth.agents.enabled"] = map[string]string{}, "namespace" // namespace only: no global value
	for a, repo := range f.Plinth.Agents.Enabled {
		n.Enabled[a] = repo
	}
	n.Image, n.From["plinth.exec.image"] = f.Plinth.Exec.Image, "namespace" // namespace only
	if err := n.Image.Check(); err != nil {
		p.problem("plinth.exec.image: %v", err)
	}
	p.validateEnabled(n.Enabled)
	p.validateNamespaceKeys(g, n.Automatic, n.ExpirationMinutes, n.UngatedAgents, n.UngatedMessages)
	p.validateExec(n.EnvFile, n.Memory)
	if len(p.problems) > 0 {
		return nil, nil, &Error{File: file, Problems: p.problems}
	}
	if len(n.Automatic) == 0 {
		p.warnings = append(p.warnings, noAutomatic+" (set in the "+n.From["reviewers.automatic"]+" config)")
	}
	return n, p.warnings, nil
}

// retiredImageRef warns about plinth.exec.image-ref, which a config from the old template still has.
const retiredImageRef = "plinth.exec.image-ref is no longer used: runs use the pinned image (plinth.exec.image, " +
	"plinthctl image publish), so remove the key"

// noAutomatic warns about an empty reviewers.automatic: legitimate (it is how a repo opts out), but then a script is
// approved without any review, so config check and the controller's log say so.
const noAutomatic = "reviewers.automatic is empty: no review runs on its own, so scripts are approved without one"

// parser carries one file's problems, warnings and environment references.
type parser struct {
	getenv       Getenv
	problems     []string
	warnings     []string
	refs         []string
	sum          [32]byte            // sha256 of the file as read (of nothing when there is none)
	placeholders map[string]bool     // keys that still hold a template placeholder
	empty        map[string][]string // key -> "${VAR} is not set" / "${VAR} is empty", for the key's problems
}

// problem records "<key>: <message>". A key that still holds a placeholder reports only that: its follow-on problems
// (e.g. "<address>" is not an IP address) would be noise. A problem of a key whose ${VAR} expanded to nothing names
// the variable, so "forgejo-url: required" says why it is empty.
func (p *parser) problem(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	key, _, ok := strings.Cut(msg, ": ")
	if ok && p.placeholders[key] && !strings.Contains(msg, "template placeholder") {
		return
	}
	if why := p.empty[key]; ok && len(why) > 0 {
		msg += " (" + strings.Join(why, ", ") + ")"
	}
	p.problems = append(p.problems, msg)
}

// noteEmpty records why a ${VAR} in key expanded to nothing.
func (p *parser) noteEmpty(key, why string) {
	if p.empty == nil {
		p.empty = map[string][]string{}
	}
	p.empty[key] = append(p.empty[key], why)
}

// placeholder records that key still holds the template placeholder ph.
func (p *parser) placeholder(key, ph string) {
	if p.placeholders == nil {
		p.placeholders = map[string]bool{}
	}
	p.placeholders[key] = true
	p.problem("%s: still holds the template placeholder %s", key, ph)
}

// load reads dir/config.yml (no symlink, regular file, at most maxSize), checks placeholders, expands ~ and ${VAR} in
// string values, and decodes it strictly into out, which already holds the defaults. A problem found while walking is
// recorded in p; an error returned means the file couldn't be read or parsed at all.
func (p *parser) load(dir string, out any) error {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("missing, and so is its directory (forgejo-url is required)")
	}
	if err != nil {
		return err
	}
	defer root.Close()
	b, err := safefs.ReadFile(root, FileName, maxSize)
	if err == nil {
		p.sum = sha256.Sum256(b)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("missing (forgejo-url is required)")
		}
		return err
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty, or comments only: sets nothing
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("more than one YAML document")
	}
	p.walk(&doc, "")
	// Decode the expanded tree strictly: yaml.Node.Decode has no KnownFields, so go through bytes.
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	strict := yaml.NewDecoder(bytes.NewReader(expanded))
	strict.KnownFields(true)
	if err := strict.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			var exp yaml.Node
			if yaml.Unmarshal(expanded, &exp) != nil {
				exp = yaml.Node{}
			}
			orig, at := nodeLines(&doc), nodeLines(&exp)
			for _, e := range te.Errors {
				p.problem("%s", located(e, orig, at))
			}
			return nil
		}
		return err
	}
	return nil
}

// nodeAt is where a key or a sequence item is in a document: its dotted path (as walk names it), its key ("" for an
// item), the line of its key and the line of its value.
type nodeAt struct {
	path, key        string
	keyLine, valLine int
}

// nodeLines lists every key and sequence item below n.
func nodeLines(n *yaml.Node) []nodeAt {
	var out []nodeAt
	var visit func(n *yaml.Node, path string)
	visit = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				visit(c, path)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				sub := fmt.Sprintf("%s[%d]", path, i)
				out = append(out, nodeAt{path: sub, keyLine: c.Line, valLine: c.Line})
				visit(c, sub)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				sub := k.Value
				if path != "" {
					sub = path + "." + k.Value
				}
				out = append(out, nodeAt{path: sub, key: k.Value, keyLine: k.Line, valLine: v.Line})
				visit(v, sub)
			}
		}
	}
	visit(n, "")
	return out
}

// strictLineRe is the line yaml.v3 puts in front of a strict-decode problem: a line of the expanded document it
// decoded, not of the file. fieldRe is its unknown-key problem.
var (
	strictLineRe = regexp.MustCompile(`^line (\d+): (.*)$`)
	fieldRe      = regexp.MustCompile(`^field (\S+) not found in type `)
)

// located is a strict-decode problem e with the key path it is about and that key's line in the file, "<path>: <what>
// (line <n>)", e.g. "ollama.mcp: field mcp not found in type config.Ollama (line 3)", so the problem belongs to its
// key (LoadPlain keeps a key's problems by that prefix) and points at the user's line. orig are the file's
// nodes, at the expanded document's, whose lines e names; both trees have the same keys, since walk changes only
// scalar values. A problem it can't place stays as it is.
func located(e string, orig, at []nodeAt) string {
	m := strictLineRe.FindStringSubmatch(e)
	if m == nil {
		return e
	}
	line, what := m[1], m[2]
	path := ""
	if f := fieldRe.FindStringSubmatch(what); f != nil {
		for _, n := range at {
			if strconv.Itoa(n.keyLine) == line && n.key == f[1] {
				path = n.path
				break
			}
		}
	} else {
		// The value on that line: a mapping's value line is its first key's too, so the deepest one; two as deep (a
		// flow mapping on one line) can't be told apart.
		depth := -1
		for _, n := range at {
			if strconv.Itoa(n.valLine) != line {
				continue
			}
			switch d := strings.Count(n.path, ".") + strings.Count(n.path, "["); {
			case d > depth:
				path, depth = n.path, d
			case d == depth:
				path = ""
			}
		}
	}
	if path == "" {
		return e
	}
	for _, n := range orig {
		if n.path == path {
			return fmt.Sprintf("%s: %s (line %d)", path, what, n.keyLine)
		}
	}
	return path + ": " + what
}

// walk checks and expands every string scalar below n; path is the dotted key path.
func (p *parser) walk(n *yaml.Node, path string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, c := range n.Content {
			sub := path
			if n.Kind == yaml.SequenceNode {
				sub = fmt.Sprintf("%s[%d]", path, i)
			}
			p.walk(c, sub)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if path != "" {
				key = path + "." + key
			}
			p.walk(n.Content[i+1], key)
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return
		}
		// A review prompt is taken literally: it may well contain ${...} or <...> itself (e.g. about bash). It is a
		// placeholder only when the whole value is one.
		if strings.HasSuffix(path, ".definition") {
			if v := strings.TrimSpace(n.Value); placeholderRe.MatchString(v) && placeholderRe.FindString(v) == v {
				p.placeholder(path, v)
			}
			return
		}
		if m := placeholderRe.FindString(n.Value); m != "" {
			p.placeholder(path, m)
		}
		if v := p.expand(n.Value, path); v != n.Value {
			n.Value, n.Style = v, yaml.DoubleQuotedStyle // quoted, so an expanded "123" stays a string
		}
	}
}

// expand replaces a leading ~ with the home directory and every ${VAR} with its value; an unset VAR becomes "" and a
// warning.
func (p *parser) expand(v, path string) string {
	v = varRe.ReplaceAllStringFunc(v, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		p.refs = append(p.refs, name)
		val, ok := p.getenv(name)
		if !ok {
			p.warnings = append(p.warnings, fmt.Sprintf("%s: ${%s} is not set, so it is empty", path, name))
			p.noteEmpty(path, "${"+name+"} is not set")
		} else if val == "" {
			p.noteEmpty(path, "${"+name+"} is empty")
		}
		return val
	})
	if v == "~" || strings.HasPrefix(v, "~/") {
		if home, ok := p.getenv("HOME"); ok && home != "" {
			v = filepath.Join(home, strings.TrimPrefix(v, "~"))
		} else {
			p.problem("%s: ~ can't be expanded, HOME is not set", path)
		}
	}
	return v
}

func (p *parser) validateGlobal(g *Global) {
	if g.ForgejoURL == "" {
		p.problem("forgejo-url: required")
	} else {
		p.httpURL("forgejo-url", g.ForgejoURL)
		g.ForgejoURL = strings.TrimSuffix(g.ForgejoURL, "/")
	}
	p.validateReviewers(g.Reviewers)
	p.validateOllama(g.Ollama)
	p.listener("plinth.mcp", g.Plinth.MCP.Listen, g.Plinth.MCP.Port, true)
	w := g.Plinth.WebUI
	p.listener("plinth.web-ui", w.Listen, w.Port, false)
	if w.PublicURL != "" {
		p.httpURL("plinth.web-ui.public-url", w.PublicURL)
	}
	p.between("plinth.web-ui.refresh-seconds", w.RefreshSeconds, 1, 3600)
	for i, u := range w.Auth.Users {
		if !loginRe.MatchString(u) {
			p.problem("plinth.web-ui.auth.users[%d]: %q is not a Forgejo login", i, u)
		}
	}
	p.between("plinth.web-ui.auth.session-hours", w.Auth.SessionHours, 1, 720)
	p.between("plinth.tui.refresh-seconds", g.Plinth.TUI.RefreshSeconds, 1, 60)
	ports := map[int]string{}
	for _, l := range []struct {
		key  string
		port int
	}{{"plinth.mcp.port", g.Plinth.MCP.Port}, {"plinth.web-ui.port", w.Port}} {
		if other, ok := ports[l.port]; ok {
			p.problem("%s: %d is also %s", l.key, l.port, other)
		}
		ports[l.port] = l.key
	}
	m := g.Plinth.Messages
	p.between("plinth.messages.refresh-seconds", m.RefreshSeconds, 1, 3600)
	if m.CatalogueURL != "" {
		p.httpURL("plinth.messages.catalogue-url", m.CatalogueURL)
	}
	p.between("plinth.runner.poll-seconds", g.Plinth.Runner.PollSeconds, 1, 60)
	p.between("plinth.runner.max-parallel-exec", g.Plinth.Runner.MaxParallelExec, 1, 16)
	p.between("plinth.runner.exec-timeout-minutes", g.Plinth.Runner.ExecTimeoutMinutes, 1, 1440)
	p.between("plinth.event-log-max", g.Plinth.EventLogMax, 1, 10_000_000)
	p.between("plinth.log.max-age-days", g.Plinth.Log.MaxAgeDays, 1, 3650)
	p.between("plinth.notice.expires", g.Plinth.Notice.Expires, 1, 3650)
	p.between("plinth.preview.limit", g.Plinth.Preview.Limit, 1, 1000)
	if g.Plinth.Notice.Expires > g.Plinth.Log.MaxAgeDays {
		p.problem("plinth.notice.expires: %d days is more than plinth.log.max-age-days (%d): a notice is read from the log files", g.Plinth.Notice.Expires, g.Plinth.Log.MaxAgeDays)
	}
	if !filepath.IsAbs(g.Plinth.AAARawDir) {
		p.problem("plinth.aaa-raw-dir: %q is not an absolute path", g.Plinth.AAARawDir)
	}
	if g.Plinth.FailedScans != nil {
		p.problem("plinth.failed-scans.repo: renamed to plinth.repo, which Save and Report both use: move the value there")
	}
	if r := g.Plinth.Repo; r != "" {
		if _, err := names.ParseNamespace(r); err != nil {
			p.problem("plinth.repo: %v", err)
		}
	}
	p.validateNamespaceKeys(g, g.Reviewers.Automatic, m.ExpirationMinutes, m.Ungated.Agents, m.Ungated.Messages)
	if g.Plinth.Exec.ImageRef != nil {
		p.warnings = append(p.warnings, retiredImageRef)
	}
	p.validateExec(g.Plinth.Exec.EnvFile, g.Plinth.Exec.Memory)
}

// validateExec checks plinth.exec: an env file that is a plain file name in the namespace's directory, not the
// config itself, and a memory limit podman takes, 6m to 64g.
func (p *parser) validateExec(envFile, memory string) {
	if envFile != "" && (filepath.Base(envFile) != envFile || envFile == "." || envFile == ".." || envFile == FileName ||
		strings.ContainsAny(envFile, "\n\x00")) {
		p.problem("plinth.exec.env-file: %q is not a file name in the namespace's directory (other than %s)", envFile, FileName)
	}
	if _, err := MemoryBytes(memory); err != nil {
		p.problem("plinth.exec.memory: %v", err)
	}
}

// validateNamespaceKeys checks the namespace keys, whichever level they come from.
func (p *parser) validateNamespaceKeys(g *Global, automatic []string, expiration int, agents, messages []string) {
	seen := map[string]bool{}
	for i, t := range automatic {
		if _, ok := g.Reviewers.Type[t]; !ok {
			p.problem("reviewers.automatic[%d]: review type %q is not defined in the global reviewers.type", i, t)
		}
		if seen[t] {
			p.problem("reviewers.automatic[%d]: %q is listed twice", i, t)
		}
		seen[t] = true
	}
	p.between("plinth.messages.expiration-minutes", expiration, 1, 10080)
	for i, a := range agents {
		if !names.ValidAgent(a) {
			p.problem("plinth.messages.ungated.agents[%d]: not an agent name (names only, never tokens; the value isn't shown)", i)
		}
	}
	for i, m := range messages {
		if !typeNameRe.MatchString(m) {
			p.problem("plinth.messages.ungated.messages[%d]: %q is not a message type name", i, m)
		}
	}
}

func (p *parser) between(key string, v, lo, hi int) {
	if v < lo || v > hi {
		p.problem("%s: %d is outside %d..%d", key, v, lo, hi)
	}
}

func (p *parser) httpURL(key, s string) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		p.problem("%s: not an http(s) URL with a host and no user:password (the value isn't shown: it may hold a password)", key)
	}
}

// listener checks an address and port; an empty address is a problem only when required.
func (p *parser) listener(key, listen string, port int, required bool) {
	if listen == "" && required {
		p.problem("%s.listen: required", key)
	} else if listen != "" && net.ParseIP(listen) == nil {
		p.problem("%s.listen: %q is not an IP address", key, listen)
	}
	p.between(key+".port", port, 1, 65535)
}

func (p *parser) validateReviewers(r GlobalReviewers) {
	for _, name := range slices.Sorted(maps.Keys(r.Type)) {
		t := r.Type[name]
		if !typeNameRe.MatchString(name) {
			p.problem("reviewers.type.%s: not a valid review type name ([a-z][a-z0-9-]{0,31})", name)
		}
		for i, u := range t.URLs {
			p.httpURL(fmt.Sprintf("reviewers.type.%s.urls[%d]", name, i), u)
		}
	}
}

func (p *parser) validateOllama(o Ollama) {
	if o.Host != "" {
		p.httpURL("ollama.host", o.Host)
		if o.Models.Review == "" || o.Models.DLP == "" {
			p.problem("ollama.models.review and .dlp: both required when ollama.host is set")
		}
	}
	p.between("ollama.timeout-seconds", o.TimeoutSeconds, 1, 3600)
	p.between("ollama.retry-seconds", o.RetrySeconds, 0, 600)
	p.between("ollama.max-parallel", o.MaxParallel, 1, 64)
}

// validateEnabled checks plinth.agents.enabled: agent names, each mapped to an absolute, clean repo path.
func (p *parser) validateEnabled(enabled map[string]string) {
	for _, a := range slices.Sorted(maps.Keys(enabled)) {
		if !names.ValidAgent(a) {
			p.problem("plinth.agents.enabled: %q is not an agent name", a)
			continue
		}
		if repo := enabled[a]; !filepath.IsAbs(repo) || filepath.Clean(repo) != repo || strings.ContainsAny(repo, "\n\x00") {
			p.problem("plinth.agents.enabled.%s: %q is not an absolute, clean path (the repo's top level)", a, repo)
		}
	}
}

var memoryRe = regexp.MustCompile(`^([1-9][0-9]{0,8})([kmg])$`)

// MemoryBytes is plinth.exec.memory in bytes: <n>k, <n>m or <n>g, from 6m (podman's least) to 64g.
func MemoryBytes(s string) (int64, error) {
	m := memoryRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not <n>k, <n>m or <n>g", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	n <<= map[string]int{"k": 10, "m": 20, "g": 30}[m[2]]
	if n < 6<<20 || n > 64<<30 {
		return 0, fmt.Errorf("%s is not between 6m and 64g", s)
	}
	return n, nil
}
