// Package doc is Plinth's YAML documents: every one starts with apiVersion: plinth/v1, kind and metadata, and
// every reader decodes it with Decode, the one strict decoder. Plinth writes its documents with its own encoders (each
// kind's Encode), every text from a script as a literal block through AppendText, so a document Plinth writes always
// decodes. Spec: plinthctl.README.md, "Documents".
package doc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// APIVersion is the schema version of every document.
const APIVersion = "plinth/v1"

// MaxSize is the largest document Decode reads (16 MiB, as the DLP scan).
const MaxSize = 16 << 20

// The kinds built so far.
const (
	KindAnswer      = "Answer"
	KindAskTemplate = "AskTemplate"
)

// Decode decodes b into v, a pointer to a kind's struct, strictly: b holds exactly one document, at most MaxSize bytes,
// with no unknown key (KnownFields), no duplicate key, no anchor, alias, merge key (<<) or explicit tag, and every key
// a plain scalar. A problem names its line.
func Decode(b []byte, v any) error {
	if len(b) > MaxSize {
		return fmt.Errorf("%d bytes, more than the %d a document may have", len(b), MaxSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var n yaml.Node
	if err := dec.Decode(&n); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("no document")
		}
		return err
	}
	var more yaml.Node
	if err := dec.Decode(&more); err == nil {
		return errors.New("more than one document")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	if err := check(&n); err != nil {
		return err
	}
	strict := yaml.NewDecoder(bytes.NewReader(b))
	strict.KnownFields(true)
	return strict.Decode(v)
}

// check refuses what yaml.v3 would accept but a document may not hold, in n and below it.
func check(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.AliasNode:
		return at(n, "an alias")
	case n.Anchor != "":
		return at(n, "an anchor")
	case n.Style&yaml.TaggedStyle != 0:
		return at(n, "an explicit tag")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			switch {
			case k.Kind != yaml.ScalarNode:
				return at(k, "a key that isn't a scalar")
			case k.Tag == "!!merge" || k.Value == "<<":
				return at(k, "a merge key (<<)")
			case seen[k.Value]:
				return at(k, "duplicate key "+strconv.Quote(k.Value))
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := check(c); err != nil {
			return err
		}
	}
	return nil
}

func at(n *yaml.Node, what string) error {
	return fmt.Errorf("line %d: %s", n.Line, what)
}

// checkEnvelope checks a document's apiVersion and kind.
func checkEnvelope(apiVersion, kind, want string) error {
	if apiVersion != APIVersion {
		return fmt.Errorf("apiVersion %q, want %s", apiVersion, APIVersion)
	}
	if kind != want {
		return fmt.Errorf("kind %q, want %s", kind, want)
	}
	return nil
}

// quote is s as a YAML double-quoted scalar: Go's escapes are YAML's (\", \\, \n, \t, \xNN, \uNNNN, \UNNNNNNNN, ...).
func quote(s string) string {
	return strconv.Quote(s)
}
