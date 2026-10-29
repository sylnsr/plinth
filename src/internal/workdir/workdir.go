// Package workdir sets up Plinth's work directory: <work> itself when Plinth starts, and a namespace's <ns-dir> when its
// repo is added. It only ever creates: every directory gets a .gitignore, every missing config.yml is written from the
// built-in template for the user to fill in, and nothing that exists is changed. Spec: plinthctl.README.md, "Layout of
// <work>".
package workdir

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/safefs"
)

// GlobalTemplate is written to <work>/config.yml when it is missing; it is also the annotated example users read.
//
//go:embed config.yml.dist
var GlobalTemplate []byte

// NamespaceTemplate is written to <ns-dir>/config.yml when it is missing; it is also the annotated example users read.
//
//go:embed ns-config.yml.dist
var NamespaceTemplate []byte

// AskTemplate is the script of the base AskTemplate of mode plinth: what <ns-dir>/ask-template.yml holds when
// EnsureNamespace writes it, and what ask.template gives an agent to start a script the controller runs from. Its one
// fixed line sources the AAA library the controller provides ($AAA_LIB). The user may edit the manifest; a script
// from an edited one has to pass ask lint --plinth like every submission.
//
//go:embed plinth.ask.sh.template
var AskTemplate []byte

// AAATemplate is the script of plain AAA's base AskTemplate (mode aaa): what new-ask starts a run from when <work> has
// no ask-template.yml. Its one fixed line sources the AAA library (plinthctl ask lib).
//
//go:embed aaa.ask.sh.template
var AAATemplate []byte

// BaseTemplateName is the AskTemplate manifest below <work> that new-ask starts plain AAA runs from (EnsureWork writes
// the base one, mode aaa, when it is missing, and the user may edit it), and the one below each <ns-dir>
// that ask.template gives (EnsureNamespace writes the base one, mode plinth).
const BaseTemplateName = "ask-template.yml"

// BaseTemplate is the base AskTemplate manifest of mode (doc.ModeAAA or doc.ModePlinth), named base: what ask template
// prints. The plinth one holds AskTemplate.
func BaseTemplate(mode string) []byte {
	script := AAATemplate
	if mode == doc.ModePlinth {
		script = AskTemplate
	}
	t := doc.AskTemplate{APIVersion: doc.APIVersion, Kind: doc.KindAskTemplate, Metadata: doc.TemplateMeta{Name: "base"},
		Spec: doc.AskTemplateSpec{Mode: mode, Script: string(script)}}
	return t.Encode()
}

// gitignore keeps a directory's contents out of git when <work> is a git repo.
const gitignore = "*\n!.gitignore\n"

// The directories below <work> and below each <ns-dir>.
var (
	workDirs = []string{"state", "aaa-raw", "logs", "requests"}
	nsDirs   = []string{"runs", "submissions", "rejected", "requests", "raw", "messages", "events", "state", "state/agents"}
)

// EnsureWork creates <work> (mode 0700), its directories, config.yml and ask-template.yml (BaseTemplate, mode aaa),
// whichever are missing, and returns what it created, relative to work. The parent of work must exist.
func EnsureWork(work string) ([]string, error) {
	var created []string
	if err := os.Mkdir(work, 0o700); err == nil {
		created = append(created, ".")
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	c, err := ensure(root, ".", workDirs, GlobalTemplate)
	created = append(created, c...)
	if err != nil {
		return created, err
	}
	made, err := writeIfMissing(root, ".", BaseTemplateName, BaseTemplate(doc.ModeAAA))
	if made {
		created = append(created, BaseTemplateName)
	}
	return created, err
}

// EnsureNamespace creates <work>/<owner>/<repo>/, its directories, config.yml and ask-template.yml (BaseTemplate, mode
// plinth), whichever are missing, and returns what it created, relative to work. <work> must exist (EnsureWork).
func EnsureNamespace(work string, ns names.Namespace) ([]string, error) {
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var created []string
	base := filepath.Join(ns.Owner, ns.Repo)
	for _, d := range []string{ns.Owner, base} {
		made, err := mkdir(root, d)
		if err != nil {
			return created, err
		}
		if made {
			created = append(created, d)
		}
	}
	c, err := ensure(root, base, nsDirs, NamespaceTemplate)
	created = append(created, c...)
	if err != nil {
		return created, err
	}
	made, err := writeIfMissing(root, base, BaseTemplateName, BaseTemplate(doc.ModePlinth))
	if made {
		created = append(created, filepath.Join(base, BaseTemplateName))
	}
	return created, err
}

// ensure creates each of dirs below base with its .gitignore, and base/config.yml from tpl, whichever are missing.
func ensure(root *os.Root, base string, dirs []string, tpl []byte) ([]string, error) {
	var created []string
	for _, d := range dirs {
		p := filepath.Join(base, d)
		made, err := mkdir(root, p)
		if err != nil {
			return created, err
		}
		if made {
			created = append(created, p)
		}
		if made, err = writeIfMissing(root, p, ".gitignore", []byte(gitignore)); err != nil {
			return created, err
		}
		if made {
			created = append(created, filepath.Join(p, ".gitignore"))
		}
	}
	made, err := writeIfMissing(root, base, "config.yml", tpl)
	if err != nil {
		return created, err
	}
	if made {
		created = append(created, filepath.Join(base, "config.yml"))
	}
	return created, nil
}

// mkdir creates dir (mode 0700) and reports whether it did. A dir that exists must be a real directory, with no
// symlink on the way to it.
func mkdir(root *os.Root, dir string) (bool, error) {
	err := root.Mkdir(dir, 0o700)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	fi, err := safefs.Lstat(root, dir)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s: not a directory", dir)
	}
	return false, nil
}

// writeIfMissing writes dir/name (mode 0600) unless something by that name exists, and reports whether it did. Another
// process creating it at the same moment (register from two launch scripts, or the controller starting) is not an error.
func writeIfMissing(root *os.Root, dir, name string, body []byte) (bool, error) {
	if _, err := root.Lstat(filepath.Join(dir, name)); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := safefs.WriteNew(root, dir, name, body, 0o600); errors.Is(err, fs.ErrExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// Namespaces lists the namespaces set up below work: every <owner>/<repo>/ directory with a config.yml or a state/
// directory, sorted. The directories of <work> itself (state/, logs/, ...) are not owners; symlinks are skipped.
func Namespaces(work string) ([]names.Namespace, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	owners, err := readDirs(root, ".")
	if err != nil {
		return nil, err
	}
	var out []names.Namespace
	for _, o := range owners {
		if slices.Contains(workDirs, o) {
			continue
		}
		repos, err := readDirs(root, o)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			ns, err := names.ParseNamespace(o + "/" + r)
			if err != nil {
				continue
			}
			for _, marker := range []string{"config.yml", "state"} {
				if _, err := safefs.Lstat(root, filepath.Join(o, r, marker)); err == nil {
					out = append(out, ns)
					break
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b names.Namespace) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

// readDirs lists the real directories (no symlinks) in dir below root.
func readDirs(root *os.Root, dir string) ([]string, error) {
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
		if e.Type().IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
