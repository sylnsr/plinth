package doc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"plinth/internal/names"
)

// The modes of an AskTemplate: plain AAA, or the controller's runs.
const (
	ModeAAA    = "aaa"
	ModePlinth = "plinth"
)

// AskTemplate is kind AskTemplate, the ask script template as a manifest: one kind with the modes aaa
// and plinth. The binary has a base one of each (workdir.BaseTemplate); <work>/ask-template.yml is the user's, which
// new-ask starts from.
type AskTemplate struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   TemplateMeta    `yaml:"metadata"`
	Spec       AskTemplateSpec `yaml:"spec"`
}

// TemplateMeta is an AskTemplate's metadata: its name, and the namespace it belongs to, if any.
type TemplateMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"` // <owner>/<repo>
}

// AskTemplateSpec is what an AskTemplate holds.
type AskTemplateSpec struct {
	Mode   string `yaml:"mode"`
	Script string `yaml:"script"`
}

// nameRe is a manifest's name: lower-case letters, digits and -, as a Kubernetes name.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Check checks what Decode can't: the envelope, the name, the namespace, the mode and that the script is one.
func (t *AskTemplate) Check() error {
	if err := checkEnvelope(t.APIVersion, t.Kind, KindAskTemplate); err != nil {
		return err
	}
	if !nameRe.MatchString(t.Metadata.Name) {
		return fmt.Errorf("metadata.name %q: lower-case letters, digits and -, at most 63", t.Metadata.Name)
	}
	if ns := t.Metadata.Namespace; ns != "" {
		if _, err := names.ParseNamespace(ns); err != nil {
			return fmt.Errorf("metadata.namespace: %w", err)
		}
	}
	if t.Spec.Mode != ModeAAA && t.Spec.Mode != ModePlinth {
		return fmt.Errorf("spec.mode %q, want %s or %s", t.Spec.Mode, ModeAAA, ModePlinth)
	}
	if !strings.HasPrefix(t.Spec.Script, "#!") {
		return errors.New("spec.script doesn't start with #!")
	}
	return nil
}

// Encode writes the manifest, its script a literal block (AppendText).
func (t *AskTemplate) Encode() []byte {
	b := fmt.Appendf(nil, "apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n", APIVersion, KindAskTemplate, quote(t.Metadata.Name))
	if t.Metadata.Namespace != "" {
		b = fmt.Appendf(b, "  namespace: %s\n", quote(t.Metadata.Namespace))
	}
	b = fmt.Appendf(b, "spec:\n  mode: %s\n  script: |2\n", quote(t.Spec.Mode))
	return AppendText(b, []byte(t.Spec.Script), 4)
}

// ParseAskTemplate decodes and checks an AskTemplate manifest.
func ParseAskTemplate(b []byte) (*AskTemplate, error) {
	var t AskTemplate
	if err := Decode(b, &t); err != nil {
		return nil, err
	}
	if err := t.Check(); err != nil {
		return nil, err
	}
	return &t, nil
}
