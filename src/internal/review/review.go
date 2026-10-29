// Package review is the Ollama review of an ask script or of an answer: the prompt of a review type (its URLs'
// contents joined in order, or its definition) is the system prompt, the model answers in a fixed shape (a JSON schema:
// a verdict and findings), and the review is Markdown that Plinth writes from that data, with the verdict under
// "## Verdict". A reply that doesn't fit fails closed. Spec: plinthctl.README.md, "Needs Ollama" (review) and Runs
// (Automatic reviews).
package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"plinth/internal/config"
	"plinth/internal/ollama"
)

// Kind is what is reviewed.
type Kind int

const (
	Script Kind = iota
	Answer
)

// AnswerMarker is what a prompt that reviews answers holds; no other prompt reviews one.
const AnswerMarker = "<<<BEGIN ANSWER>>>"

// ErrNotForAnswers: the type's prompt doesn't review answers (it has no AnswerMarker).
var ErrNotForAnswers = errors.New("its prompt doesn't review answers (no " + AnswerMarker + " section)")

const (
	maxPrompt   = 1 << 20
	maxFindings = 50
	maxText     = 1000
	numCtx      = 32768
)

// Verdicts, in the model's words and in the review's.
var verdicts = map[string]string{"ok-to-run": "OK to run", "run-after-fixes": "Run after fixes", "do-not-run": "Do not run"}

// Severities a finding may have.
var severities = []string{"high", "medium", "low"}

var schema = json.RawMessage(`{"type":"object","properties":{` +
	`"verdict":{"type":"string","enum":["ok-to-run","run-after-fixes","do-not-run"]},` +
	`"findings":{"type":"array","items":{"type":"object","properties":{` +
	`"line":{"type":"integer"},"severity":{"type":"string","enum":["high","medium","low"]},"text":{"type":"string"},` +
	`"quote":{"type":"string"}},"required":["severity","text","quote"]}}},"required":["verdict","findings"]}`)

// shape is appended to every prompt: the reply's form is Plinth's, whatever the prompt asks for.
const shape = "\n\nAnswer only with JSON matching the schema: \"verdict\" is ok-to-run, run-after-fixes or do-not-run; " +
	"\"findings\" lists each problem with the \"line\" it is on (0 when it is on none), a \"severity\" (high, medium or low) " +
	"and a short \"text\". An empty findings list is fine."

// quoteScript and quoteAnswer end the shape: a finding on a script's line quotes it, so Plinth can check that the model read
// what it says it read; an answer is never quoted, since it may hold what a DLP scan is looking for.
const (
	quoteScript = " For a finding on a line, \"quote\" is that line's text copied exactly from the script (empty for a finding on no line)."
	quoteAnswer = " \"quote\" must be empty: never copy text of the answer."
)

// Prompt is the text of a review type: its URLs' contents joined in order, or its definition. source says where it
// came from, for the review's footer.
type Prompt struct {
	Text   string
	Source string
	SHA256 string
}

// Fetcher reads prompt URLs: http(s), no redirect, at most 1 MiB, with Plinth's forgejo-token only for forgejo-url's
// scheme and host. A Forgejo /src/ page URL is read as its /raw/ file.
type Fetcher struct {
	ForgejoURL string
	Token      string
	http       *http.Client
}

// NewFetcher returns a Fetcher for forgejo-url and forgejo-token ("" for none).
func NewFetcher(forgejoURL, token string) *Fetcher {
	return &Fetcher{ForgejoURL: forgejoURL, Token: token, http: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var srcRe = regexp.MustCompile(`/src/(branch|tag|commit)/`)

// get reads one prompt URL.
func (f *Fetcher) get(ctx context.Context, raw string) ([]byte, error) {
	raw = srcRe.ReplaceAllString(raw, "/raw/$1/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%q is not an http(s) URL", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if fu, err := url.Parse(f.ForgejoURL); err == nil && f.Token != "" && fu.Scheme == u.Scheme && strings.EqualFold(fu.Host, u.Host) {
		req.Header.Set("Authorization", "token "+f.Token)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s: %v", raw, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPrompt+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", raw, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%s: HTTP %d: not found, or forgejo-token can't read it", raw, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", raw, resp.StatusCode)
	case len(b) > maxPrompt:
		return nil, fmt.Errorf("%s: larger than %d bytes", raw, maxPrompt)
	}
	return b, nil
}

// Load is the prompt of review type name.
func (f *Fetcher) Load(ctx context.Context, name string, t config.ReviewType) (Prompt, error) {
	var parts []string
	src := "the definition of reviewers.type." + name
	if len(t.URLs) > 0 {
		for _, u := range t.URLs {
			b, err := f.get(ctx, u)
			if err != nil {
				return Prompt{}, fmt.Errorf("the prompt of review type %s: %w", name, err)
			}
			parts = append(parts, string(b))
		}
		src = strings.Join(t.URLs, ", ")
	} else {
		parts = []string{t.Definition}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		return Prompt{}, fmt.Errorf("the prompt of review type %s is empty (%s)", name, src)
	}
	h := sha256.Sum256([]byte(text))
	return Prompt{Text: text, Source: src, SHA256: hex.EncodeToString(h[:])}, nil
}

// Input is what one review reviews.
type Input struct {
	Type string // the review type
	Kind Kind
	Name string // the file name the review names, e.g. ask.sh
	Body []byte
}

// File is the review's file name: review.<type>.md, review.answer.<type>.md for an answer, .FAILED.md when failed.
func (in Input) File(failed bool) string {
	n := "review." + in.Type
	if in.Kind == Answer {
		n = "review.answer." + in.Type
	}
	if failed {
		return n + ".FAILED.md"
	}
	return n + ".md"
}

// Result is a finished review: the Markdown to write and the verdict in it.
type Result struct {
	Markdown []byte
	Verdict  string // "OK to run", "Run after fixes" or "Do not run"
	// Findings are the findings, one line each as in the Markdown: "line 3 (high): text" or "(low): text".
	Findings []string
}

// Run reviews in with prompt p on model; now dates the footer. An error is the reason of the FAILED file (Failed).
func Run(ctx context.Context, c ollama.Chatter, model string, p Prompt, in Input, now func() time.Time) (*Result, error) {
	if in.Kind == Answer && !strings.Contains(p.Text, AnswerMarker) {
		return nil, ErrNotForAnswers
	}
	if len(bytes.TrimSpace(in.Body)) == 0 {
		return nil, fmt.Errorf("%s is empty", in.Name)
	}
	text := string(in.Body)
	var user string
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if in.Kind == Answer { // numbered lines, so a finding names a line without quoting its value
		var b strings.Builder
		fmt.Fprintf(&b, "Review this answer file (file: %s). Each line starts with its line number and a TAB.\n\n%s\n", in.Name, AnswerMarker)
		for i, l := range lines {
			fmt.Fprintf(&b, "%d\t%s\n", i+1, l)
		}
		b.WriteString("<<<END ANSWER>>>\n")
		user = b.String()
	} else {
		user = fmt.Sprintf("Review this script (file: %s).\n\n<<<BEGIN SCRIPT>>>\n%s\n<<<END SCRIPT>>>\n", in.Name, strings.TrimSuffix(text, "\n"))
	}
	var verdict string
	var findings []finding
	var bad error
	for range 2 {
		r, err := c.Chat(ctx, ollama.ChatRequest{Model: model, System: p.Text + shapeFor(in.Kind), Prompt: user, Format: schema,
			Options: map[string]any{"temperature": 0.2, "num_ctx": numCtx}})
		if err != nil {
			return nil, err
		}
		if r.DoneReason == "length" {
			bad = errors.New("the model's answer was cut off")
			continue
		}
		if verdict, findings, bad = parse(r.Content, lines, in.Kind == Script); bad == nil {
			break
		}
	}
	if bad != nil {
		return nil, fmt.Errorf("Ollama (model %s): %w", model, bad)
	}
	h := sha256.Sum256(in.Body)
	var b strings.Builder
	fmt.Fprintf(&b, "# Review %s of %s\n\n## Verdict\n%s\n\n## Findings\n", in.Type, in.Name, verdict)
	if len(findings) == 0 {
		b.WriteString("None.\n")
	}
	var texts []string
	for _, f := range findings {
		if f.line > 0 {
			texts = append(texts, fmt.Sprintf("line %d (%s): %s", f.line, f.severity, f.text))
		} else {
			texts = append(texts, fmt.Sprintf("(%s): %s", f.severity, f.text))
		}
		fmt.Fprintf(&b, "- %s\n", texts[len(texts)-1])
	}
	fmt.Fprintf(&b, "\n---\n_Review of %s (sha256 %s) by review type '%s' (prompt sha256 %s, from %s) on %s, %s. Written by plinthctl from "+
		"the model's data; no agent was in the loop. A review is advice from a model, not an approval.", in.Name, hex.EncodeToString(h[:]),
		in.Type, p.SHA256, p.Source, model, now().UTC().Format(time.RFC3339))
	if in.Kind == Answer {
		b.WriteString(" It may quote the answer: handle it like the answer itself.")
	}
	b.WriteString("_\n")
	return &Result{Markdown: []byte(b.String()), Verdict: verdict, Findings: texts}, nil
}

type finding struct {
	line     int
	severity string
	text     string
}

// parse checks the model's reply against the schema and returns the verdict in the review's words and the findings,
// each text on one line, printable, cut to maxText characters.
func parse(content string, lines []string, script bool) (string, []finding, error) {
	var reply struct {
		Verdict  *string `json:"verdict"`
		Findings *[]struct {
			Line     *int    `json:"line"`
			Severity *string `json:"severity"`
			Text     *string `json:"text"`
			Quote    *string `json:"quote"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(content), &reply); err != nil || reply.Verdict == nil || reply.Findings == nil {
		return "", nil, errors.New("the model's answer is not the expected JSON")
	}
	v, ok := verdicts[*reply.Verdict]
	if !ok {
		return "", nil, fmt.Errorf("the model's verdict %s is none of ok-to-run, run-after-fixes, do-not-run", strconv.Quote(clean(*reply.Verdict, 40)))
	}
	if len(*reply.Findings) > maxFindings {
		return "", nil, fmt.Errorf("the model gave %d findings, more than %d", len(*reply.Findings), maxFindings)
	}
	var out []finding
	for _, f := range *reply.Findings {
		switch {
		case f.Severity == nil || f.Text == nil:
			return "", nil, errors.New("a finding without a severity or a text")
		case !slices.Contains(severities, *f.Severity):
			return "", nil, fmt.Errorf("a finding's severity %s is none of high, medium, low", strconv.Quote(clean(*f.Severity, 40)))
		case f.Line != nil && (*f.Line < 0 || *f.Line > len(lines)):
			return "", nil, fmt.Errorf("a finding on line %d, outside the file's %d lines", *f.Line, len(lines))
		}
		fd := finding{severity: *f.Severity, text: clean(*f.Text, maxText)}
		if f.Line != nil {
			fd.line = *f.Line
		}
		if fd.text == "" {
			return "", nil, errors.New("a finding with an empty text")
		}
		if script && f.Quote != nil {
			if at, ok := locate(lines, fd.line, *f.Quote); !ok {
				fd.text = "(unverified: the quoted text is not in the script) " + fd.text
			} else if at != fd.line {
				if fd.line > 0 {
					fd.text += fmt.Sprintf(" [line corrected: the model said %d]", fd.line)
				}
				fd.line = at
			}
		}
		out = append(out, fd)
	}
	return v, out, nil
}

// shapeFor is the shape appended to a prompt that reviews a kind of file.
func shapeFor(k Kind) string {
	if k == Answer {
		return shape + quoteAnswer
	}
	return shape + quoteScript
}

// locate checks the quote of a finding on line (0: on none) against the script's lines. It returns the line the quote is
// on and whether it was found: the finding's own line when it holds the quote, else the first line that does, else (a
// quote over several lines) the finding's line when the script holds it as a whole. A quote of under 3 characters says
// too little to check and counts as found.
func locate(lines []string, line int, quote string) (int, bool) {
	q := squash(quote)
	if len([]rune(q)) < 3 {
		return line, true
	}
	if line >= 1 && strings.Contains(squash(lines[line-1]), q) {
		return line, true
	}
	for i, l := range lines {
		if strings.Contains(squash(l), q) {
			return i + 1, true
		}
	}
	return line, strings.Contains(squash(strings.Join(lines, "\n")), q)
}

// squash is s with every run of whitespace one space, so a quote matches whatever the model did to the indentation.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// clean is s on one line: every run of whitespace and control characters one space, cut to n characters.
func clean(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	return s
}

// Failed is the body of a FAILED review: why first (the UIs show that line), then what to do.
func Failed(in Input, why, source, model string, now time.Time) []byte {
	what := "script"
	if in.Kind == Answer {
		what = "answer"
	}
	if source == "" {
		source = "-"
	}
	if model == "" {
		model = "-"
	}
	return fmt.Appendf(nil, "# Review %s of %s: FAILED\n\n%s.\n\nNo review exists for this %s from this review type; its absence is not a sign the %s is fine.\n\n- Prompt: %s\n- Model: %s\n- When: %s\n",
		in.Type, in.Name, strings.TrimSuffix(why, "."), what, what, source, model, now.UTC().Format(time.RFC3339))
}
