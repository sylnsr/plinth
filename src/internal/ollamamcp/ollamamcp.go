// Package ollamamcp is the Ollama gateway: the one way to Ollama, for Plinth's own callers only (the
// runners' reviews and DLP scans, plinthctl review, dlp-scan and ollama status). It is the Ollama MCP, the tools
// ollama_chat, ollama_list_models, ollama_ps, ollama_show and ollama_version over MCP/HTTP (package mcp), on a Unix
// socket below <work>, in a container of its own (Gateway), answered by the one Ollama client (package ollama), so the
// semaphore, the timeouts and the retry are the same for every caller. Client is the callers' side. Spec:
// plinthctl.README.md, "Ollama gateway".
package ollamamcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"plinth/internal/logfile"
	"plinth/internal/mcp"
	"plinth/internal/ollama"
)

// Path is the MCP endpoint on the socket.
const Path = "/mcp"

// SocketDir is the gateway's directory below <work>, the only part of <work> mounted into its container; SocketName is
// its socket there (mode 0600, in a directory of mode 0700).
const (
	SocketDir  = "state/ollama"
	SocketName = "ollama.sock"
)

// maxInFlight bounds the calls the gateway takes at once.
const maxInFlight = 256

// Options are what the server needs besides its socket.
type Options struct {
	// Client is the Ollama client, which must be configured.
	Client *ollama.Client

	// Version is the serverInfo version.
	Version string

	// Log, when set, gets a line per tool call (the tool, how long, how it ended; never its arguments or its result).
	Log *logfile.Log
}

const chatSchema = `{"type":"object","properties":{` +
	`"model":{"type":"string","description":"the model to ask, as ollama_list_models names it"},` +
	`"prompt":{"type":"string","description":"the user message"},` +
	`"system":{"type":"string","description":"the system prompt"},` +
	`"format":{"type":["string","object"],"description":"json, or a JSON schema the reply must follow (structured output)"},` +
	`"options":{"type":"object","description":"Ollama model options, e.g. temperature, seed, num_ctx"},` +
	`"keep_alive":{"type":"string","description":"how long the model stays loaded after the call, e.g. 5m"}},` +
	`"required":["model","prompt"],"additionalProperties":false}`

const showSchema = `{"type":"object","properties":{"model":{"type":"string","description":"the model to describe"}},` +
	`"required":["model"],"additionalProperties":false}`

const noArgsSchema = `{"type":"object","properties":{},"additionalProperties":false}`

type tools struct {
	c *ollama.Client
}

// New is the handler of the MCP endpoint (Path).
func New(o Options) http.Handler {
	t := &tools{c: o.Client}
	// The calls wait at the client's semaphore (ollama.max-parallel), so many may be in flight: every review and DLP scan
	// of every namespace queues here.
	s := &mcp.Server{Name: "plinth-ollama-gateway", Version: o.Version, Log: o.Log, MaxInFlight: maxInFlight}
	s.Tools = []mcp.Tool{
		{Name: "ollama_chat", Description: "Ask a model one question (one user message, optionally a system prompt and a format) and return its reply as JSON: model, reply, done_reason (length: cut off), eval_count, total_duration_ms.", Schema: []byte(chatSchema), Call: t.chat},
		{Name: "ollama_list_models", Description: "List the models Ollama has (its /api/tags).", Schema: []byte(noArgsSchema), Call: t.get("/api/tags")},
		{Name: "ollama_ps", Description: "List the models Ollama has loaded now (its /api/ps).", Schema: []byte(noArgsSchema), Call: t.get("/api/ps")},
		{Name: "ollama_show", Description: "Describe one model: its modelfile, parameters, template and details (its /api/show).", Schema: []byte(showSchema), Call: t.show},
		{Name: "ollama_version", Description: "Ollama's version (its /api/version).", Schema: []byte(noArgsSchema), Call: t.get("/api/version")},
	}
	mux := http.NewServeMux()
	mux.Handle(Path, s)
	return mux
}

type chatArgs struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	System    string          `json:"system,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive string          `json:"keep_alive,omitempty"`
}

type chatResult struct {
	Model           string `json:"model"`
	Reply           string `json:"reply"`
	DoneReason      string `json:"done_reason"`
	EvalCount       int    `json:"eval_count"`
	TotalDurationMS int64  `json:"total_duration_ms"`
}

func (t *tools) chat(ctx context.Context, args json.RawMessage) (string, error) {
	var a chatArgs
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Model == "" || a.Prompt == "" {
		return "", errors.New("arguments: model and prompt are required")
	}
	if f := string(a.Format); f == "null" {
		a.Format = nil
	} else if len(a.Format) > 0 && a.Format[0] != '{' && f != `"json"` {
		return "", errors.New(`arguments: format is "json" or a JSON schema (an object)`)
	}
	rep, err := t.c.Chat(ctx, ollama.ChatRequest{Model: a.Model, System: a.System, Prompt: a.Prompt, Format: a.Format, Options: a.Options,
		KeepAlive: a.KeepAlive})
	if err != nil {
		return "", err
	}
	return mcp.JSONText(chatResult{rep.Model, rep.Content, rep.DoneReason, rep.EvalCount, rep.TotalDuration.Milliseconds()})
}

// get is the tool that returns Ollama's answer to GET path as it is.
func (t *tools) get(path string) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var none struct{}
		if err := mcp.DecodeArgs(args, &none); err != nil {
			return "", err
		}
		raw, err := t.c.Raw(ctx, path, nil)
		return string(raw), err
	}
}

func (t *tools) show(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Model string `json:"model"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Model == "" {
		return "", errors.New("arguments: model is required")
	}
	body, err := mcp.JSONText(map[string]string{"model": a.Model})
	if err != nil {
		return "", err
	}
	raw, err := t.c.Raw(ctx, "/api/show", []byte(body))
	return string(raw), err
}

// Listen listens on dir's socket (SocketName): a left-over socket goes first, and the new one gets mode 0600.
func Listen(dir string) (net.Listener, error) {
	path := filepath.Join(dir, SocketName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("the gateway's socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve serves h on ln until ctx is cancelled, then lets the requests in flight end for 5 s (their contexts are ctx's, so
// a call waiting for Ollama stops at once). It returns nil after such a stop, or the error that ended the listener.
// There is no write timeout: a model call takes as long as ollama.timeout-seconds allows.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if srv.Shutdown(sctx) != nil {
		srv.Close()
	}
	<-errc
	return nil
}
