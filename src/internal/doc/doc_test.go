package doc

import (
	"strings"
	"testing"
)

const answerDoc = `apiVersion: plinth/v1
kind: Answer
metadata:
  run: "calm-otter-16"
  started: "2026-10-19T14:00:00Z"
sections:
  - title: |2
      1. uname
    output: |2
      Linux host 6.1
result: ok
`

func TestDecode(t *testing.T) {
	var a Answer
	if err := Decode([]byte(answerDoc), &a); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(true); err != nil || a.Metadata.Run != "calm-otter-16" || len(a.Sections) != 1 ||
		a.Sections[0].Title != "1. uname\n" || a.Sections[0].Output != "Linux host 6.1\n" || a.Result != ResultOK {
		t.Errorf("%+v %v", a, err)
	}
	for _, c := range []struct{ name, doc, want string }{
		{"empty", "", "no document"},
		{"two documents", answerDoc + "---\nkind: Answer\n", "more than one document"},
		{"an empty second document", answerDoc + "---\n", "more than one document"},
		{"unknown key", answerDoc + "dlp: ok\n", "field dlp not found"},
		{"unknown nested key", strings.Replace(answerDoc, "  run:", "  runs: x\n  run:", 1), "field runs not found"},
		{"duplicate key", answerDoc + "result: fail\n", `duplicate key "result"`},
		{"anchor", strings.Replace(answerDoc, "metadata:", "metadata: &m", 1), "an anchor"},
		{"explicit tag", strings.Replace(answerDoc, "kind: Answer", "kind: !!str Answer", 1), "an explicit tag"},
		{"merge key", strings.Replace(answerDoc, "  run:", "  <<: {started: x}\n  run:", 1), "merge key"},
		{"not a mapping", "- a\n", "cannot unmarshal"},
		{"too large", answerDoc + "#" + strings.Repeat("x", MaxSize) + "\n", "more than the"},
	} {
		var a Answer
		if err := Decode([]byte(c.doc), &a); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestAnswerCheck(t *testing.T) {
	for _, c := range []struct {
		name       string
		edit       func(a *Answer)
		fromScript bool
		want       string
	}{
		{"ok", func(*Answer) {}, true, ""},
		{"killed", func(a *Answer) { a.Result = "" }, true, ""},
		{"apiVersion", func(a *Answer) { a.APIVersion = "plinth/v2" }, true, "apiVersion"},
		{"kind", func(a *Answer) { a.Kind = "Review" }, true, "kind"},
		{"run", func(a *Answer) { a.Metadata.Run = "../x" }, true, "not a run name"},
		{"started", func(a *Answer) { a.Metadata.Started = "yesterday" }, true, "RFC 3339"},
		{"result", func(a *Answer) { a.Result = "maybe" }, true, "result"},
		{"failure without fail", func(a *Answer) { a.Failure = "x\n" }, true, "failure"},
		{"status from a script", func(a *Answer) { a.Status = &AnswerStatus{DLP: &DLP{Result: ResultOK}} }, true, "status"},
		{"status from Plinth", func(a *Answer) { a.Status = &AnswerStatus{DLP: &DLP{Result: ResultOK}} }, false, ""},
		{"bad DLP result", func(a *Answer) { a.Status = &AnswerStatus{DLP: &DLP{Result: "skipped"}} }, false, "status.dlp.result"},
	} {
		var a Answer
		if err := Decode([]byte(answerDoc), &a); err != nil {
			t.Fatal(err)
		}
		c.edit(&a)
		if err := a.Check(c.fromScript); (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// wantText is what a text decodes to after AppendText: the escaped lines, ending in one \n (trailing empty lines
// dropped, as a literal block's clip does), or "".
func wantText(b []byte) string {
	s := strings.TrimRight(string(AppendText(nil, b, 0)), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// textSeeds are hostile texts: control characters, invalid UTF-8, YAML-looking lines, document markers, CRLF, the
// line breaks YAML knows besides \n, long lines and nothing.
var textSeeds = []string{"", "a", "a\n", "\n\nx\n\n", "---\n...\n--- x", "key: value\n- item\n? q\n: a", "|2\n  >-\n'\"",
	"\x00\x01\x07\x1b[31mred\x1b[0m\r\nnext\r", "\xff\xfe\xc3", "  \u0085\ufeff\ufffe\uffff\u007f\u0080",
	"\t\ttab\n\t", "   \n  \n", "#not a comment\n  # indented", strings.Repeat("x", 70000), "héllo wörld ✓ 日本",
	"\\x41 already escaped", "  - title: |2\nresult: ok\nstatus:\n  dlp: {result: ok}"}

func TestAnswerEncode(t *testing.T) {
	for _, s := range textSeeds {
		a := Answer{APIVersion: APIVersion, Kind: KindAnswer, Metadata: AnswerMeta{Run: "calm-otter", Started: "2026-10-19T14:00:00+02:00"},
			Sections: []Section{{Title: s, Output: s}, {Title: "2", Output: ""}}, Failure: s, Result: ResultFail,
			Status: &AnswerStatus{DLP: &DLP{Result: ResultFail, Model: "qwen3:8b", Chunks: 1, Fixed: 2, Masked: 3, Reason: s}}}
		var got Answer
		if err := Decode(a.Encode(), &got); err != nil {
			t.Fatalf("%q: %v\n%s", s, err, a.Encode())
		}
		w := wantText([]byte(s))
		if err := got.Check(false); err != nil || got.Sections[0].Title != w || got.Sections[0].Output != w || got.Sections[1].Title != "2\n" ||
			got.Sections[1].Output != "" || got.Failure != w || got.Result != ResultFail || got.Metadata != a.Metadata {
			t.Errorf("%q: %+v %v", s, got, err)
		}
		if d := got.Status.DLP; d.Result != ResultFail || d.Model != "qwen3:8b" || d.Chunks != 1 || d.Fixed != 2 || d.Masked != 3 || d.Reason != w {
			t.Errorf("%q: status %+v", s, d)
		}
	}
	var got Answer
	if err := Decode((&Answer{APIVersion: APIVersion, Kind: KindAnswer, Metadata: AnswerMeta{Run: "calm-otter"}}).Encode(), &got); err != nil ||
		len(got.Sections) != 0 || got.Status != nil {
		t.Errorf("no sections: %+v %v", got, err)
	}
}

// FuzzText: whatever a script prints, the AAA library's layout (the sections' |2 blocks at column 6, the failure's at
// column 2) decodes into the schema, each text as wantText says.
func FuzzText(f *testing.F) {
	for _, s := range textSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		d := []byte("apiVersion: plinth/v1\nkind: Answer\nmetadata:\n  run: \"calm-otter\"\nsections:\n  - title: |2\n")
		d = AppendText(d, b, 6)
		d = append(d, "    output: |2\n"...)
		d = AppendText(d, b, 6)
		d = append(d, "failure: |2\n"...)
		d = AppendText(d, b, 2)
		d = append(d, "result: fail\n"...)
		var a Answer
		if err := Decode(d, &a); err != nil {
			t.Fatalf("%q: %v\n%s", b, err, d)
		}
		w := wantText(b)
		if err := a.Check(true); err != nil || len(a.Sections) != 1 || a.Sections[0].Title != w || a.Sections[0].Output != w || a.Failure != w {
			t.Errorf("%q: %+v %v", b, a, err)
		}
	})
}

func TestWriteText(t *testing.T) {
	var out strings.Builder
	if err := WriteText(&out, strings.NewReader("a\x01\nb"), 2); err != nil || out.String() != "  a\\x01\n  b\n" {
		t.Errorf("%q %v", out.String(), err)
	}
	out.Reset()
	if err := WriteText(&out, strings.NewReader(strings.Repeat("x", MaxSize+5)), 0); err != nil ||
		!strings.HasSuffix(out.String(), "x\n[cut: more than 16777216 bytes]\n") || out.Len() != MaxSize+len("\n[cut: more than 16777216 bytes]\n") {
		t.Errorf("cut: %d %v", out.Len(), err)
	}
}

func TestAskTemplate(t *testing.T) {
	tpl := AskTemplate{APIVersion: APIVersion, Kind: KindAskTemplate, Metadata: TemplateMeta{Name: "base", Namespace: "michael/devops"},
		Spec: AskTemplateSpec{Mode: ModeAAA, Script: "#!/bin/bash\n# AAA script: x\n\tindented: |\n---\n"}}
	got, err := ParseAskTemplate(tpl.Encode())
	if err != nil || *got != tpl {
		t.Fatalf("%+v %v\n%s", got, err, tpl.Encode())
	}
	for _, c := range []struct {
		name string
		edit func(t *AskTemplate)
		want string
	}{
		{"mode", func(t *AskTemplate) { t.Spec.Mode = "k8s" }, "spec.mode"},
		{"name", func(t *AskTemplate) { t.Metadata.Name = "Base" }, "metadata.name"},
		{"namespace", func(t *AskTemplate) { t.Metadata.Namespace = "devops" }, "metadata.namespace"},
		{"script", func(t *AskTemplate) { t.Spec.Script = "echo hi\n" }, "#!"},
	} {
		bad := tpl
		c.edit(&bad)
		if _, err := ParseAskTemplate(bad.Encode()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := ParseAskTemplate([]byte("apiVersion: plinth/v1\nkind: AskTemplate\nmetadata:\n  name: x\nspec:\n  mode: aaa\n  script: \"#!\"\n  extra: 1\n")); err == nil {
		t.Error("an unknown key passed")
	}
}
