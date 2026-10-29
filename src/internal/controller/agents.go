package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"plinth/internal/agentmcp"
	"plinth/internal/logfile"
	"plinth/internal/names"
)

// serveAgents starts the Plinth MCP server, the agents' way in, on Options.MCPListener or on
// plinth.mcp.listen:plinth.mcp.port; a port it can't listen on keeps the controller from starting. It returns the
// function that stops it, which waits a moment for the calls in progress (an ask_submit waits for its runner).
func (p *process) serveAgents() (stop func(), err error) {
	ln := p.MCPListener
	if ln == nil {
		addr := net.JoinHostPort(p.g.Plinth.MCP.Listen, strconv.Itoa(p.g.Plinth.MCP.Port))
		if ln, err = net.Listen("tcp", addr); err != nil {
			return nil, fmt.Errorf("the Plinth MCP server (plinth.mcp.listen, plinth.mcp.port): %w", err)
		}
	}
	s := &agentmcp.Server{Work: p.Work, Global: p.g, Version: p.Version, Namespace: p.namespace, Log: p.log.Component(logfile.AgentMCP)}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Component(logfile.AgentMCP).Error("the Plinth MCP server stopped", "err", err)
		}
	}()
	url := fmt.Sprintf("http://%s%s", ln.Addr(), agentmcp.Path)
	p.mu.Lock()
	p.mcpURL = url
	p.mu.Unlock()
	p.log.Component(logfile.AgentMCP).Info("the Plinth MCP server is listening", "url", url)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
		<-done
	}, nil
}

// namespace is the runner of the attached namespace ns, for the Plinth MCP server's goroutines; nil when it isn't
// attached.
func (p *process) namespace(ns names.Namespace) agentmcp.Namespace {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l, ok := p.attached[ns.String()]; ok {
		return l.Runner
	}
	return nil
}
