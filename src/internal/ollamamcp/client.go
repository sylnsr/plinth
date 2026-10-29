package ollamamcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"plinth/internal/mcp"
	"plinth/internal/ollama"
)

// Client is the callers' side of the gateway of a <work>: an ollama.Chatter for the reviews and the DLP scan, and what
// ollama status asks. Every call is a tool call over the socket; the gateway's errors come back as they are
// ("Ollama: HTTP 404: ...", "not available without Ollama (...)"). A call that finds nobody at the socket fails with
// "the Ollama gateway isn't running (plinthctl ollama-mcp start)".
type Client struct {
	configured bool
	mcp        *mcp.Client
}

// NewClient is the client of work's gateway; configured is whether ollama.host is set (without it every call fails
// with ollama.ErrNotConfigured, and no gateway runs).
func NewClient(work string, configured bool) *Client {
	sock := SocketPath(work)
	// A connection per call: one kept to a gateway since replaced would fail the next POST, which isn't retried.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	return &Client{configured: configured, mcp: &mcp.Client{HTTP: &http.Client{Transport: tr}, URL: "http://ollama-gateway" + Path}}
}

// Configured reports whether ollama.host is set.
func (c *Client) Configured() bool { return c.configured }

// call is one tool call: its text, or the tool's error as it is.
func (c *Client) call(ctx context.Context, tool string, args any) (string, error) {
	if !c.configured {
		return "", ollama.ErrNotConfigured
	}
	text, err := c.mcp.Call(ctx, tool, args)
	var te *mcp.ToolError
	switch {
	case err == nil:
		return text, nil
	case errors.As(err, &te):
		return "", errors.New(te.Text)
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return "", errNotRunning
	}
	return "", fmt.Errorf("the Ollama gateway: %w", err)
}

// Chat is ollama_chat.
func (c *Client) Chat(ctx context.Context, r ollama.ChatRequest) (*ollama.ChatReply, error) {
	text, err := c.call(ctx, "ollama_chat", chatArgs{Model: r.Model, Prompt: r.Prompt, System: r.System, Format: r.Format,
		Options: r.Options, KeepAlive: r.KeepAlive})
	if err != nil {
		return nil, err
	}
	var res chatResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return nil, fmt.Errorf("the Ollama gateway: ollama_chat's result is not JSON: %w", err)
	}
	return &ollama.ChatReply{Model: res.Model, Content: res.Reply, DoneReason: res.DoneReason, EvalCount: res.EvalCount,
		TotalDuration: time.Duration(res.TotalDurationMS) * time.Millisecond}, nil
}

// Version is ollama_version: Ollama's version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	err := c.callJSON(ctx, "ollama_version", &out)
	return out.Version, err
}

// Tags is ollama_list_models: the models Ollama has.
func (c *Client) Tags(ctx context.Context) ([]ollama.Model, error) {
	var out struct {
		Models []ollama.Model `json:"models"`
	}
	err := c.callJSON(ctx, "ollama_list_models", &out)
	return out.Models, err
}

// PS is ollama_ps: the models loaded now.
func (c *Client) PS(ctx context.Context) ([]ollama.Model, error) {
	var out struct {
		Models []ollama.Model `json:"models"`
	}
	err := c.callJSON(ctx, "ollama_ps", &out)
	return out.Models, err
}

func (c *Client) callJSON(ctx context.Context, tool string, v any) error {
	text, err := c.call(ctx, tool, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		return fmt.Errorf("the Ollama gateway: %s's result is not JSON: %w", tool, err)
	}
	return nil
}
