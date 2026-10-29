// Package ollama is Plinth's one Ollama client, which only the Ollama gateway (package ollamamcp) holds: timeouts
// (ollama.timeout-seconds), one retry after ollama.retry-seconds for a transient error (unreachable, HTTP 5xx, an empty
// reply), and one semaphore (ollama.max-parallel) around the model calls. Every failure is an error that names its
// cause. The reviews and the DLP scan ask a Chatter: the gateway's client. Spec: plinthctl.README.md, "Ollama".
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"plinth/internal/config"
)

// maxBody bounds a reply from Ollama.
const maxBody = 16 << 20

// ErrNotConfigured: ollama.host is not set.
var ErrNotConfigured = errors.New("not available without Ollama (ollama.host is not set)")

// ErrUnavailable wraps the error of an Ollama that can't be reached: "not available without Ollama (...)".
var ErrUnavailable = errors.New("not available without Ollama")

// Chatter is what the reviews and the DLP scan ask: the Ollama gateway's client (ollamamcp.Client) or, in the gateway
// and in tests, a Client. Without ollama.host, Chat fails with ErrNotConfigured.
type Chatter interface {
	Configured() bool
	Chat(ctx context.Context, r ChatRequest) (*ChatReply, error)
}

// Client is Ollama at Host ("" for not configured). Its zero value is not usable: New makes one.
type Client struct {
	Host    string
	timeout time.Duration
	retry   time.Duration
	sem     chan struct{}
	http    *http.Client
}

// New returns the client for o. It follows no redirect.
func New(o config.Ollama) *Client {
	n := max(o.MaxParallel, 1)
	return &Client{
		Host:    strings.TrimSuffix(o.Host, "/"),
		timeout: time.Duration(o.TimeoutSeconds) * time.Second,
		retry:   time.Duration(o.RetrySeconds) * time.Second,
		sem:     make(chan struct{}, n),
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// Configured reports whether ollama.host is set.
func (c *Client) Configured() bool { return c.Host != "" }

// ChatRequest is one /api/chat call, without streaming.
type ChatRequest struct {
	Model     string
	System    string          // "" for none
	Prompt    string          // the user message
	Format    json.RawMessage // a JSON schema for structured output; nil for free text
	Options   map[string]any  // e.g. temperature, seed, num_ctx
	KeepAlive string          // "" for Ollama's default
}

// ChatReply is what /api/chat answered.
type ChatReply struct {
	Model         string
	Content       string
	DoneReason    string // "stop"; "length" when the reply was cut off
	EvalCount     int
	TotalDuration time.Duration
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string      `json:"model"`
	Message chatMessage `json:"message"`
	Reason  string      `json:"done_reason"`
	Eval    int         `json:"eval_count"`
	Total   int64       `json:"total_duration"`
}

// Chat asks the model, behind the semaphore. An empty reply is transient: retried once.
func (c *Client) Chat(ctx context.Context, r ChatRequest) (*ChatReply, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if r.Model == "" {
		return nil, errors.New("Ollama: no model")
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	body := map[string]any{"model": r.Model, "stream": false}
	var msgs []chatMessage
	if r.System != "" {
		msgs = append(msgs, chatMessage{"system", r.System})
	}
	body["messages"] = append(msgs, chatMessage{"user", r.Prompt})
	if r.Format != nil {
		body["format"] = r.Format
	}
	if r.Options != nil {
		body["options"] = r.Options
	}
	if r.KeepAlive != "" {
		body["keep_alive"] = r.KeepAlive
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out chatResponse
	err = c.call(ctx, http.MethodPost, "/api/chat", b, func(raw []byte) error {
		out = chatResponse{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("Ollama: the reply is not JSON: %w", err)
		}
		if strings.TrimSpace(out.Message.Content) == "" {
			return errTransient{errors.New("Ollama: empty reply")}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ChatReply{Model: out.Model, Content: out.Message.Content, DoneReason: out.Reason, EvalCount: out.Eval,
		TotalDuration: time.Duration(out.Total)}, nil
}

// Version is GET /api/version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	err := c.getJSON(ctx, "/api/version", &out)
	return out.Version, err
}

// Model is one model of /api/tags or /api/ps.
type Model struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Tags is GET /api/tags: the models Ollama has.
func (c *Client) Tags(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	err := c.getJSON(ctx, "/api/tags", &out)
	return out.Models, err
}

// PS is GET /api/ps: the models loaded now.
func (c *Client) PS(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	err := c.getJSON(ctx, "/api/ps", &out)
	return out.Models, err
}

// Raw is GET path, or POST path with body, as Ollama answered it (the Ollama MCP passes it on).
func (c *Client) Raw(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	var out json.RawMessage
	err := c.call(ctx, method, path, body, func(raw []byte) error {
		if !json.Valid(raw) {
			return errors.New("Ollama: the reply is not JSON")
		}
		out = raw
		return nil
	})
	return out, err
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	return c.call(ctx, http.MethodGet, path, nil, func(raw []byte) error {
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("Ollama: the reply is not JSON: %w", err)
		}
		return nil
	})
}

// errTransient marks an error that is worth one retry.
type errTransient struct{ error }

func (e errTransient) Unwrap() error { return e.error }

// call sends one request and hands a 2xx reply to parse; a transient error, from the request or from parse, is tried
// once more after the retry time.
func (c *Client) call(ctx context.Context, method, path string, body []byte, parse func([]byte) error) error {
	err := c.once(ctx, method, path, body, parse)
	var t errTransient
	if !errors.As(err, &t) || ctx.Err() != nil {
		return unwrap(err)
	}
	select {
	case <-time.After(c.retry):
	case <-ctx.Done():
		return ctx.Err()
	}
	return unwrap(c.once(ctx, method, path, body, parse))
}

// unwrap drops the transient mark: the caller sees the cause.
func unwrap(err error) error {
	var t errTransient
	if errors.As(err, &t) {
		return t.error
	}
	return err
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, parse func([]byte) error) error {
	parent := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Host+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil && parent.Err() != nil {
		return parent.Err()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return c.noReply()
		}
		return errTransient{fmt.Errorf("%w (%s unreachable: %v)", ErrUnavailable, c.Host, cause(err))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		switch {
		case parent.Err() != nil:
			return parent.Err()
		case ctx.Err() != nil:
			return c.noReply() // the deadline ran out while the reply came: a timeout too
		}
		return errTransient{fmt.Errorf("Ollama: reading the reply: %w", err)}
	}
	if len(raw) > maxBody {
		return fmt.Errorf("Ollama: the reply is larger than %d bytes", maxBody)
	}
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("Ollama: HTTP %d%s", resp.StatusCode, errorText(raw))
		if resp.StatusCode >= 500 {
			return errTransient{err}
		}
		return err
	}
	return parse(raw)
}

// noReply is the error of a call that got no whole reply within ollama.timeout-seconds (Timeout). Not retried: it would
// double the wait.
func (c *Client) noReply() error {
	return fmt.Errorf("Ollama: no reply within %s (ollama.timeout-seconds)", c.timeout)
}

// cause is the error of a failed request without the URL that net/http puts around it.
func cause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// errorText is ": <error>" from Ollama's {"error": ...}, cut to 200 printable characters; "" without one.
func errorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Error == "" {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, e.Error)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return ": " + s
}
