package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxReply bounds a reply the client reads.
const maxReply = 32 << 20

// Client calls the tools of an MCP server like Server: one POST per call, without initialize first (the server is
// stateless). HTTP is the transport, e.g. one that dials a Unix socket.
type Client struct {
	HTTP  *http.Client
	URL   string
	Token string // sent as Authorization: Bearer when set
}

// ToolError is a tool's error: a result with isError set. Its text is the tool's error, which names its cause.
type ToolError struct{ Text string }

func (e *ToolError) Error() string { return e.Text }

// Call calls the tool name with args (marshalled as its arguments; nil for none) and returns the text of its result.
func (c *Client) Call(ctx context.Context, name string, args any) (string, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	body, err := marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return "", fmt.Errorf("MCP: reading the reply: %w", err)
	}
	if len(raw) > maxReply {
		return "", fmt.Errorf("MCP: the reply is larger than %d bytes", maxReply)
	}
	var r response
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &r) != nil {
		return "", fmt.Errorf("MCP: HTTP %d: %s", resp.StatusCode, printable(raw, 200))
	}
	if r.Error != nil {
		return "", fmt.Errorf("MCP: %s (%d)", r.Error.Message, r.Error.Code)
	}
	var cr callResult
	if err := json.Unmarshal(r.Result, &cr); err != nil {
		return "", errors.New("MCP: the result is not a tool's result")
	}
	var text strings.Builder
	for _, t := range cr.Content {
		text.WriteString(t.Text)
	}
	if cr.IsError {
		return "", &ToolError{text.String()}
	}
	return text.String(), nil
}

// printable is b cut to n bytes, with control characters as spaces.
func printable(b []byte, n int) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b[:min(len(b), n)]))
	return strings.TrimSpace(s)
}
