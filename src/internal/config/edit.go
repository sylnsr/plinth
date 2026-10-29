package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"plinth/internal/names"
	"plinth/internal/safefs"
)

// EnableAgent records in the namespace's config.yml that agent is enabled for repo, the top level its registration
// comes from: plinth.agents.enabled.<agent>. DisableAgent removes that entry and reports whether there was one. These
// are what plinthctl agent enable and disable (and later the TUI and the web UI) write.
//
// The file is edited by key through the yaml.v3 node tree: every other key and every comment stays. A file with no
// document yet (the template: comments only) keeps its text as it is and gets the block appended. The result must
// load (LoadNamespace, with g); otherwise the file is put back as it was and the error says why.
func EnableAgent(work string, ns names.Namespace, g *Global, getenv Getenv, agent, repo string) error {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo || strings.ContainsAny(repo, "\n\x00") {
		return fmt.Errorf("repo %q: not an absolute, clean path", repo)
	}
	_, err := editEnabled(work, ns, g, getenv, agent, repo)
	return err
}

// DisableAgent: see EnableAgent.
func DisableAgent(work string, ns names.Namespace, g *Global, getenv Getenv, agent string) (bool, error) {
	return editEnabled(work, ns, g, getenv, agent, "")
}

// editEnabled sets plinth.agents.enabled.<agent> to repo, or removes it when repo is "", and reports whether the file
// changed.
func editEnabled(work string, ns names.Namespace, g *Global, getenv Getenv, agent, repo string) (bool, error) {
	if !names.ValidAgent(agent) {
		return false, fmt.Errorf("agent %q: not an agent name", agent)
	}
	return editNamespace(work, ns, g, getenv,
		"Agents the user enabled, each with the repo its registration must come from (plinthctl agent enable).",
		func(top *yaml.Node) (bool, error) { return setEnabled(top, agent, repo) })
}

// SetImage writes the pin, plinth.exec.image, into the namespace's config.yml, edited by key as EnableAgent does: what
// image publish and image use write.
func SetImage(work string, ns names.Namespace, g *Global, getenv Getenv, pin ImagePin) error {
	if pin == (ImagePin{}) {
		return errors.New("no image to pin")
	}
	if err := pin.Check(); err != nil {
		return err
	}
	_, err := editNamespace(work, ns, g, getenv,
		"The image this namespace's runs use, pinned by digest (plinthctl image publish and image use write it).",
		func(top *yaml.Node) (bool, error) { return setImage(top, pin) })
	return err
}

// editNamespace edits the namespace's config.yml by key: set changes the top mapping and reports whether it did. A
// file with no document yet keeps its text and gets the new block appended, with comment above it. The result must
// load; otherwise the file is put back as it was.
func editNamespace(work string, ns names.Namespace, g *Global, getenv Getenv, comment string, set func(top *yaml.Node) (bool, error)) (bool, error) {
	if _, err := names.ParseNamespace(ns.String()); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return false, err
	}
	defer root.Close()
	dir := filepath.Join(ns.Owner, ns.Repo)
	file := filepath.Join(dir, FileName)
	old, err := safefs.ReadFile(root, file, maxSize)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	var doc yaml.Node
	err = yaml.NewDecoder(bytes.NewReader(old)).Decode(&doc)
	empty := errors.Is(err, io.EOF) || (err == nil && len(doc.Content) == 0)
	if err != nil && !empty {
		return false, fmt.Errorf("%s doesn't parse: %w", filepath.Join(work, file), err)
	}
	var out []byte
	if empty {
		top := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		changed, err := set(top)
		if err != nil || !changed {
			return false, err
		}
		if out, err = appendBlock(old, top, comment); err != nil {
			return false, err
		}
	} else {
		top := doc.Content[0]
		if top.Kind != yaml.MappingNode {
			return false, fmt.Errorf("%s: the document is not a mapping", filepath.Join(work, file))
		}
		changed, err := set(top)
		if err != nil || !changed {
			return false, err
		}
		if out, err = encode(&doc); err != nil {
			return false, err
		}
	}
	if err := safefs.Replace(root, dir, FileName, out, 0o600); err != nil {
		return false, err
	}
	if _, _, lerr := LoadNamespace(work, ns, g, getenv); lerr != nil {
		if old == nil {
			err = safefs.Remove(root, file)
		} else {
			err = safefs.Replace(root, dir, FileName, old, 0o600)
		}
		return false, errors.Join(fmt.Errorf("the edited config doesn't load, so it was put back: %w", lerr), err)
	}
	return true, nil
}

// setEnabled sets or removes (repo "") the entry for agent in top's plinth.agents.enabled, making the mappings on the
// way when it sets one, and reports whether anything changed.
func setEnabled(top *yaml.Node, agent, repo string) (bool, error) {
	n, err := mapping(top, repo != "", "plinth", "agents", "enabled")
	if err != nil || n == nil {
		return false, err
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != agent {
			continue
		}
		if repo == "" {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return true, nil
		}
		if n.Content[i+1].Kind == yaml.ScalarNode && n.Content[i+1].Value == repo {
			return false, nil
		}
		n.Content[i+1] = scalar(repo)
		return true, nil
	}
	if repo == "" {
		return false, nil
	}
	n.Style = 0 // block style, also for a mapping written as {}
	n.Content = append(n.Content, scalar(agent), scalar(repo))
	return true, nil
}

// setImage sets plinth.exec.image in top to pin, making the mappings on the way, and reports whether anything
// changed.
func setImage(top *yaml.Node, pin ImagePin) (bool, error) {
	exec, err := mapping(top, true, "plinth", "exec")
	if err != nil {
		return false, err
	}
	want := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		scalar("ref"), scalar(pin.Ref), scalar("commit"), scalar(pin.Commit), scalar("tree"), scalar(pin.Tree)}}
	if old := child(exec, "image"); old != nil {
		var cur ImagePin
		if old.Kind == yaml.MappingNode && old.Decode(&cur) == nil && cur == pin {
			return false, nil
		}
		want.HeadComment, want.LineComment, want.FootComment = old.HeadComment, old.LineComment, old.FootComment
		*old = *want
		return true, nil
	}
	exec.Style = 0 // block style, also for a mapping written as {}
	exec.Content = append(exec.Content, scalar("image"), want)
	return true, nil
}

// mapping walks the keys down from top and returns the mapping at their end. With create it makes a missing (or empty)
// one on the way; without, it returns nil for one that isn't there.
func mapping(top *yaml.Node, create bool, keys ...string) (*yaml.Node, error) {
	n := top
	for _, key := range keys {
		next := child(n, key)
		if next == nil || isNull(next) {
			if !create {
				return nil, nil
			}
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				n.Style = 0
				n.Content = append(n.Content, scalar(key), next)
			} else { // "key:" with no value: the node in the tree becomes the mapping, its comments kept
				*next = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: next.HeadComment,
					LineComment: next.LineComment, FootComment: next.FootComment}
			}
		}
		if next.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: %s is not a mapping", strings.Join(keys, "."), key)
		}
		n = next
	}
	return n, nil
}

// child returns the value of key in the mapping n, nil if n has no such key (or isn't a mapping).
func child(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// appendBlock appends top, with comment above it, to a file that has no document yet, leaving its text as it is.
func appendBlock(old []byte, top *yaml.Node, comment string) ([]byte, error) {
	top.Content[0].HeadComment = comment
	block, err := encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{top}})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(old)
	if len(old) > 0 {
		if !bytes.HasSuffix(old, []byte("\n")) {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	b.Write(block)
	return b.Bytes(), nil
}

// encode writes doc with the two-space indent of Plinth's templates.
func encode(doc *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
