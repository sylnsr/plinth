// Package mcp is the JSON-RPC 2.0 core of Plinth's MCP servers: MCP over HTTP, stateless (no session id, no server
// push), one JSON message per POST. The Ollama gateway (package ollamamcp) is built on it, and the Plinth MCP server
// will be; Client is a caller's side. Spec: plinthctl.README.md, "Ollama gateway".
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"plinth/internal/logfile"
)

// ProtocolVersion is the MCP version the server speaks when a client asks for one it doesn't know.
const ProtocolVersion = "2025-06-18"

// supported are the versions a client may ask for: the tools/list and tools/call messages are the same in all of them.
var supported = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// MaxBody bounds a request.
const MaxBody = 8 << 20

// defaultInFlight is Server.MaxInFlight when it is not set.
const defaultInFlight = 32

// JSON-RPC 2.0 error codes.
const codeParse = -32700

const codeInvalidRequest = -32600

const codeMethodNotFound = -32601

const codeInvalidParams = -32602

const codeInternal = -32603

// Tool is one tool the server offers.
type Tool struct {
	Name string

	Description string

	// Schema is the JSON Schema of the arguments: an object.
	Schema json.RawMessage

	// Call runs the tool. args is the call's "arguments" as sent (empty when there are none). The text it returns is the
	// result; an error becomes a result with isError set and the error's text, so the caller sees why.
	Call func(ctx context.Context, args json.RawMessage) (string, error)

	// ReadOnly says the tool changes nothing (annotations.readOnlyHint): a client may ask the user less before it calls it.
	ReadOnly bool
}

// Server is an MCP server over HTTP: the handler of the MCP endpoint. Its fields are set before the first request.
type Server struct {
	// Name and Version are the serverInfo of the initialize result.
	Name string

	Version string

	// Instructions, when set, are the initialize result's instructions: what the client tells its model about the
	// server as a whole.
	Instructions string

	// Tools are what tools/list offers.
	Tools []Tool

	// Gate, when set, is asked first about every request: a status other than 0 refuses it with that status and message
	// (401 adds WWW-Authenticate: Bearer).
	Gate func(r *http.Request) (status int, message string)

	// Log, when set, gets one line per tool call: its name, how long it took and how it ended. Never its arguments or its
	// result.
	Log *logfile.Log

	// MaxInFlight bounds the requests handled at once (0: 32); more get 503.
	MaxInFlight int

	once   sync.Once
	list   json.RawMessage
	byName map[string]*Tool
	slots  chan struct{}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

type toolInfo struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

type toolAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint"`
}

type toolList struct {
	Tools []toolInfo `json:"tools"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

func (s *Server) init() {
	n := s.MaxInFlight
	if n <= 0 {
		n = defaultInFlight
	}
	s.slots = make(chan struct{}, n)
	s.byName = map[string]*Tool{}
	infos := make([]toolInfo, 0, len(s.Tools))
	for i := range s.Tools {
		t := &s.Tools[i]
		s.byName[t.Name] = t
		info := toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
		if t.ReadOnly {
			info.Annotations = &toolAnnotations{ReadOnlyHint: true}
		}
		infos = append(infos, info)
	}
	b, err := marshal(toolList{infos})
	if err != nil {
		panic("mcp: a tool's schema is not valid JSON: " + err.Error())
	}
	s.list = b
}

// ServeHTTP handles one POST: a JSON-RPC request gets its response as the body, a notification gets 202 and no body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(s.init)
	w.Header().Set("Cache-Control", "no-store")
	if s.Gate != nil {
		if status, msg := s.Gate(r); status != 0 {
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			http.Error(w, msg, status)
			return
		}
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// A page in a browser can send a text/plain POST to any address it can reach without asking first; JSON it can't.
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many requests at once", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "can't read the request", http.StatusBadRequest)
		}
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{codeInvalidRequest, "batches are not supported"})
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{codeParse, "not a JSON object"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{codeInvalidRequest, "not a JSON-RPC 2.0 request"})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted) // a notification: nothing to answer
		return
	}
	if b := req.ID[0]; b != '"' && b != '-' && (b < '0' || b > '9') {
		reply(w, http.StatusBadRequest, nil, nil, &rpcError{codeInvalidRequest, "id must be a string or a number"})
		return
	}
	res, rerr := s.dispatch(r.Context(), &req)
	reply(w, http.StatusOK, req.ID, res, rerr)
}

func reply(w http.ResponseWriter, status int, id, res json.RawMessage, e *rpcError) {
	b, err := marshal(response{"2.0", id, res, e})
	if err != nil {
		http.Error(w, "can't encode the reply", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func (s *Server) dispatch(ctx context.Context, req *request) (json.RawMessage, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, &rpcError{codeInvalidParams, "params: " + err.Error()}
			}
		}
		v := ProtocolVersion
		if slices.Contains(supported, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return result(initResult{v, map[string]any{"tools": map[string]any{}}, serverInfo{s.Name, s.Version}, s.Instructions})
	case "ping":
		return json.RawMessage("{}"), nil
	case "tools/list":
		return s.list, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "params: " + err.Error()}
		}
		t, ok := s.byName[p.Name]
		if !ok {
			return nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name)}
		}
		start := time.Now()
		text, err := call(ctx, t, p.Arguments)
		cr := callResult{[]textContent{{"text", text}}, false}
		if err != nil {
			cr = callResult{[]textContent{{"text", err.Error()}}, true}
		}
		if s.Log != nil {
			outcome := "ok"
			if err != nil {
				outcome = "error: " + err.Error()
			}
			if err != nil {
				s.Log.Warn("tool call", "tool", p.Name, "took", time.Since(start).Round(time.Millisecond).String(), "outcome", outcome)
			} else {
				s.Log.Info("tool call", "tool", p.Name, "took", time.Since(start).Round(time.Millisecond).String(), "outcome", outcome)
			}
		}
		return result(cr)
	}
	return nil, &rpcError{codeMethodNotFound, fmt.Sprintf("unknown method %q", req.Method)}
}

// call runs the tool; a panic in it is an error of the call, not the end of the process.
func call(ctx context.Context, t *Tool, args json.RawMessage) (text string, err error) {
	defer func() {
		if v := recover(); v != nil {
			text, err = "", fmt.Errorf("internal error: %v", v)
		}
	}()
	return t.Call(ctx, args)
}

func result(v any) (json.RawMessage, *rpcError) {
	b, err := marshal(v)
	if err != nil {
		return nil, &rpcError{codeInternal, err.Error()}
	}
	return b, nil
}

// DecodeArgs decodes a call's arguments into v, a pointer to a struct: no arguments is {}, and a field v doesn't have
// is an error, so a misspelt argument is told instead of ignored.
func DecodeArgs(args json.RawMessage, v any) error {
	args = bytes.TrimSpace(args)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("arguments: more than one JSON value")
	}
	return nil
}

// JSONText is v as compact JSON text without escaping <, > and &: a tool's result is read by people too.
func JSONText(v any) (string, error) {
	b, err := marshal(v)
	return string(b), err
}

// marshal is json.Marshal without escaping <, > and & (which it does inside a json.RawMessage too): replies are read by
// people as well.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Bearer is the token of the request's Authorization: Bearer header, "" without one.
func Bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

// SameKey reports whether got is want, in constant time (both are hashed first, so their lengths don't show). An empty
// want is never matched.
func SameKey(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return want != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
