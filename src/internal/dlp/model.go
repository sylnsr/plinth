package dlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"

	"plinth/internal/ollama"
)

// The model step: the text is cut into chunks of numbered lines and the model answers only with line numbers and a
// fixed kind (structured output, temperature 0); Scan masks those lines itself, so the model never rewrites text and
// versions and gate values stay exact.
const (
	chunkEntries = 120 // numbered entries per chunk
	pieceBytes   = 400 // a longer line goes to the model in pieces of this size, each with its line's number
	numCtx       = 16384
)

// Kinds are the kinds of secret the model may report.
var Kinds = []string{"password", "api_key_or_token", "private_key", "auth_header", "credential_in_url", "encryption_key",
	"session_cookie", "other_secret"}

var schema = func() json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{"findings": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"line": map[string]any{"type": "integer"}, "kind": map[string]any{"type": "string", "enum": Kinds}},
			"required":   []string{"line", "kind"},
		}}},
		"required": []string{"findings"},
	})
	if err != nil {
		panic(err)
	}
	return b
}()

const system = "You are a data-loss-prevention scanner for infrastructure command output. " +
	`You receive numbered lines in the form "<number><TAB><text>"; a long line comes in several pieces with the same number. ` +
	"Report the numbers of lines that contain an actual secret VALUE that must not leave the LAN: " +
	"passwords, API keys, access tokens, bearer or basic auth headers, private key material, encryption or LUKS keys, " +
	"cloud access keys, session cookies, credentials embedded in URLs, long random-looking secret strings. " +
	"Do NOT report lines that only mention a word like password or token without a value, nor hostnames, IP addresses, " +
	"ports, versions, UUIDs, file paths, device serial numbers, counts or timestamps. " +
	"If you are unsure whether a long random string is a secret, report it. " +
	"Answer only with JSON matching the schema. An empty findings list is the normal case."

// Result is what a scan did.
type Result struct {
	Model  string
	Fixed  int // fixed-pattern masks
	Chunks int // chunks the model read
	Lines  int // lines the model flagged, masked
}

// Scan is the whole DLP scan of b: the fixed patterns (Fixed), then the lines the model flags in what they left, each
// replaced by "[line N masked by dlp-scan: <kind>]". A line is what ends in \n (a last line without it too). On an
// error the text is returned with the fixed-pattern masks alone, for a caller that keeps it (the controller), and the error
// names the cause; an Ollama that is not configured or can't be reached says "not available without Ollama".
func Scan(ctx context.Context, c ollama.Chatter, model string, b []byte) ([]byte, Result, error) {
	fixed, n := Fixed(b)
	res := Result{Model: model, Fixed: n}
	if !c.Configured() {
		return fixed, res, ollama.ErrNotConfigured
	}
	lines := splitLines(fixed)
	masked, err := scanLines(ctx, c, model, lines, &res)
	if err != nil {
		return fixed, res, err
	}
	res.Lines = len(masked)
	if len(masked) == 0 {
		return fixed, res, nil
	}
	return maskLines(lines, masked, 0, bytes.HasSuffix(fixed, []byte("\n"))), res, nil
}

// ScanTexts is Scan of several texts at once, the values of a document: the fixed patterns mask each text on its
// own (a private-key block shrinks to one line, so a value couldn't be cut back out of one joined text), and the
// model reads the lines of all of them in one series of chunks, numbered across the texts, so a document costs no
// more model calls than one text of all its lines. A flagged line is masked in its own text, by its number there. On
// an error the texts come back with the fixed-pattern masks alone.
func ScanTexts(ctx context.Context, c ollama.Chatter, model string, texts [][]byte) ([][]byte, Result, error) {
	res := Result{Model: model}
	out := make([][]byte, len(texts))
	split := make([][][]byte, len(texts))
	var all [][]byte
	for i, t := range texts {
		var n int
		out[i], n = Fixed(t)
		res.Fixed += n
		split[i] = splitLines(out[i])
		all = append(all, split[i]...)
	}
	if !c.Configured() {
		return out, res, ollama.ErrNotConfigured
	}
	masked, err := scanLines(ctx, c, model, all, &res)
	if err != nil {
		return out, res, err
	}
	res.Lines = len(masked)
	first := 0
	for i, lines := range split {
		for l := range lines {
			if _, ok := masked[first+l+1]; ok {
				out[i] = maskLines(lines, masked, first, bytes.HasSuffix(out[i], []byte("\n")))
				break
			}
		}
		first += len(lines)
	}
	return out, res, nil
}

// splitLines cuts b into lines: what ends in \n, and a last line without it.
func splitLines(b []byte) [][]byte {
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1] // the newline at the end, not a line
	}
	return lines
}

// scanLines is the model step over lines, numbered from 1: it returns the numbers of the lines the model flagged
// with their kind, and counts the chunks it asked about in res.
func scanLines(ctx context.Context, c ollama.Chatter, model string, lines [][]byte, res *Result) (map[int]string, error) {
	type entry struct {
		line  int
		piece []byte
	}
	var entries []entry
	for i, l := range lines {
		for first := true; first || len(l) > 0; first = false {
			cut := min(len(l), pieceBytes)
			for cut < len(l) && cut > 0 && !utf8.RuneStart(l[cut]) {
				cut--
			}
			if cut == 0 && len(l) > 0 {
				cut = min(len(l), pieceBytes) // no rune start: invalid UTF-8, cut anywhere
			}
			entries = append(entries, entry{i + 1, l[:cut]})
			l = l[cut:]
		}
	}
	masked := map[int]string{}
	for from := 0; from < len(entries); from += chunkEntries {
		chunk := entries[from:min(from+chunkEntries, len(entries))]
		var text bytes.Buffer
		var nums []int
		for i, e := range chunk {
			if i > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(strconv.Itoa(e.line) + "\t")
			text.Write(e.piece)
			nums = append(nums, e.line)
		}
		found, err := scanChunk(ctx, c, model, text.String(), nums[0], nums[len(nums)-1])
		if err != nil {
			return nil, err
		}
		for l, k := range found {
			if _, ok := masked[l]; !ok {
				masked[l] = k
			}
		}
		res.Chunks++
	}
	return masked, nil
}

// maskLines joins lines, each line i whose number first+i+1 is in masked replaced by "[line <i+1> masked by
// dlp-scan: <kind>]", and ends the text in \n when nl is set.
func maskLines(lines [][]byte, masked map[int]string, first int, nl bool) []byte {
	var out bytes.Buffer
	for i, l := range lines {
		if k, ok := masked[first+i+1]; ok {
			fmt.Fprintf(&out, "[line %d masked by dlp-scan: %s]", i+1, k)
		} else {
			out.Write(l)
		}
		if i < len(lines)-1 || nl {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// scanChunk asks the model about one chunk, lines first to last, and returns the lines it flagged with their kind. A
// reply that doesn't fit (not the schema, a line outside the chunk, an unknown kind, cut off) is asked once more.
func scanChunk(ctx context.Context, c ollama.Chatter, model, text string, first, last int) (map[int]string, error) {
	var bad error
	for range 2 {
		r, err := c.Chat(ctx, ollama.ChatRequest{Model: model, System: system, Prompt: text, Format: schema,
			Options: map[string]any{"temperature": 0, "seed": 0, "num_ctx": numCtx}})
		if err != nil {
			return nil, err
		}
		if r.DoneReason == "length" {
			bad = errors.New("the model's answer was cut off")
			continue
		}
		var top map[string]json.RawMessage
		var findings []struct {
			Line *int    `json:"line"`
			Kind *string `json:"kind"`
		}
		if json.Unmarshal([]byte(r.Content), &top) != nil || top["findings"] == nil || string(top["findings"]) == "null" ||
			json.Unmarshal(top["findings"], &findings) != nil {
			bad = errors.New("the model's answer is not the expected JSON")
			continue
		}
		found := map[int]string{}
		bad = nil
		for _, f := range findings {
			if f.Line == nil || f.Kind == nil || *f.Line < first || *f.Line > last || !slices.Contains(Kinds, *f.Kind) {
				bad = errors.New("the model reported a line outside the chunk or an unknown kind")
				break
			}
			if _, ok := found[*f.Line]; !ok {
				found[*f.Line] = *f.Kind
			}
		}
		if bad == nil {
			return found, nil
		}
	}
	return nil, &ReplyError{fmt.Errorf("Ollama (model %s): %w", model, bad)}
}

// ReplyError is a scan that reached the model, but whose reply didn't fit, twice: the scan ran and failed, unlike
// one that couldn't ask the model (Ollama not configured or unreachable, the gateway not running), which can be tried
// again.
type ReplyError struct{ Err error }

func (e *ReplyError) Error() string {
	return e.Err.Error()
}

func (e *ReplyError) Unwrap() error {
	return e.Err
}
