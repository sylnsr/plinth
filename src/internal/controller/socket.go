package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"plinth/internal/names"
	"plinth/internal/runner"
)

// Socket is the controller's Unix socket, below <work>: mode 0600, and only a peer with the controller's own uid is
// served. A terminal asks over it to start a run there (StartTerminal), which the namespace's runner carries out
// (runner.TerminalOp); controller status and stop ask it what it is doing and to stop (Query, RequestStop).
const Socket = "state/plinth.sock"

// Status is what a running controller says about itself, for controller status and start.
type Status struct {
	PID        int               `json:"pid"`
	Started    time.Time         `json:"started"`
	Work       string            `json:"work"`
	MCP        string            `json:"mcp,omitempty"` // the Plinth MCP server's URL, once it listens
	Ready      bool              `json:"ready"`         // its first poll is done
	Ollama     bool              `json:"ollama"`        // ollama.host is set, so it keeps the Ollama gateway running
	Namespaces []NamespaceStatus `json:"namespaces"`
	Running    []string          `json:"running"` // the runs whose script still runs, "<ns> <agent>/<RUN>": stop refuses while there are any
}

// NamespaceStatus is an attached namespace and its runner's last heartbeat (zero before the first).
type NamespaceStatus struct {
	Name      string    `json:"name"`
	Heartbeat time.Time `json:"heartbeat"`
}

// termRequest is what the terminal sends: first "exec", then "end" with how the script ended. controller status
// sends "status", controller stop "stop".
type termRequest struct {
	Op     string `json:"op"`
	NS     string `json:"ns,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Run    string `json:"run,omitempty"`
	Result string `json:"result,omitempty"`
	Force  bool   `json:"force,omitempty"` // stop: even while runs run, which the stop then kills
}

// termReply is the controller's answer to "exec": the runner's (runner.TerminalReply, the same fields) as JSON.
type termReply struct {
	OK     bool   `json:"ok"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
	Script string `json:"script,omitempty"` // the run directory's ask.sh, absolute: what runs
	Sum    string `json:"sum,omitempty"`    // its sha256, as checked right before the start
	Dir    string `json:"dir,omitempty"`    // the run directory, absolute
	Lib    string `json:"lib,omitempty"`    // the run's AAA library, absolute: $AAA_LIB
}

// termOp is one "exec" on its way from handle to its namespace's runner, through the main loop (route).
type termOp struct {
	ns names.Namespace
	runner.TerminalOp
}

// termWait is how long handle waits for the namespace's runner; tests shorten it.
var termWait = time.Minute

// listen opens the socket: a left-over file goes first (the controller holds <work>'s lock, so no other controller uses it).
func (p *process) listen() (*net.UnixListener, error) {
	path := filepath.Join(p.Work, filepath.FromSlash(Socket))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serve takes the socket's connections until it is closed.
func (p *process) serve(ln *net.UnixListener) {
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

// handle serves one connection: a peer with another uid is dropped unanswered; an "exec" goes to the main loop, which
// passes it on to its namespace's runner.
func (p *process) handle(conn *net.UnixConn) {
	if uid, err := peerUID(conn); err != nil || uid != os.Getuid() {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	var req termRequest
	line, err := r.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	ns, nerr := names.ParseNamespace(req.NS)
	switch {
	case err != nil:
		conn.Close()
		return
	case req.Op == "status":
		send(conn, p.status())
		conn.Close()
		return
	case req.Op == "stop":
		running := p.running()
		if len(running) > 0 && !req.Force {
			reply(conn, termReply{Reason: "still running: " + strings.Join(running, ", ")})
			conn.Close()
			return
		}
		p.log.Info("stopping: controller stop asked over the socket", "force", req.Force, "running", strings.Join(running, ","))
		reply(conn, termReply{OK: true})
		conn.Close()
		p.stop() // as Ctrl-C: Run's loop stops the server and the runners (and their running scripts), then releases the lock
		return
	case req.Op != "exec" || nerr != nil || !names.ValidAgent(req.Agent) || !names.ValidRun(req.Run):
		reply(conn, termReply{Reason: "not an exec of a namespace's run"})
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	replyc := make(chan runner.TerminalReply, 1)
	op := termOp{ns: ns, TerminalOp: runner.TerminalOp{Agent: req.Agent, Run: req.Run, Reply: replyc, Turn: make(chan struct{}, 1),
		End: func() (string, bool) { return terminalEnd(r) }, Close: conn.Close}}
	op.Turn <- struct{}{}
	select {
	case p.termc <- op:
	case <-p.ctx.Done():
		conn.Close()
		return
	}
	var rep runner.TerminalReply
	select {
	case rep = <-replyc:
	case <-time.After(termWait):
		if !op.Claim() {
			rep = <-replyc // the runner took the op up: its answer comes
			break
		}
		rep = runner.TerminalReply{Reason: "the controller didn't answer within a minute; nothing started"}
	case <-p.ctx.Done():
		if !op.Claim() {
			rep = <-replyc
			break
		}
		conn.Close()
		return
	}
	reply(conn, termReply(rep))
	if !rep.OK {
		conn.Close()
	}
}

// terminalEnd reads the terminal's report of how the script ended, after the runner's OK: false when the connection
// ended without one.
func terminalEnd(r *bufio.Reader) (string, bool) {
	var req termRequest
	line, err := r.ReadBytes('\n')
	if err == nil && json.Unmarshal(line, &req) == nil && req.Op == "end" {
		return req.Result, true
	}
	return "", false
}

// route passes an "exec" from handle to its namespace's runner (the main loop owns the attached namespaces).
func (p *process) route(op termOp) {
	l := p.attached[op.ns.String()]
	if l == nil {
		op.Reply <- runner.TerminalReply{Reason: fmt.Sprintf("namespace %s is not attached to the controller (plinthctl controller status says why)", op.ns)}
		return
	}
	if !l.Terminal(op.TerminalOp) {
		op.Reply <- runner.TerminalReply{Reason: fmt.Sprintf("namespace %s is busy with another start in a terminal; try again", op.ns)}
	}
}

// peerUID is the uid of the process at the other end of conn (SO_PEERCRED).
func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if serr != nil {
		return -1, serr
	}
	return int(cred.Uid), nil
}

func reply(conn net.Conn, rep termReply) { send(conn, rep) }

// send writes v as one line of JSON.
func send(conn net.Conn, v any) {
	b, _ := json.Marshal(v)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	conn.Write(append(b, '\n'))
}

// status is what the process says about itself (Status).
func (p *process) status() Status {
	p.mu.Lock()
	st := Status{PID: os.Getpid(), Started: p.started, Work: p.Work, MCP: p.mcpURL, Ready: p.ready,
		Ollama: p.s.Ollama.Configured(), Namespaces: []NamespaceStatus{}}
	for _, key := range slices.Sorted(maps.Keys(p.attached)) {
		n := NamespaceStatus{Name: key}
		l := p.attached[key]
		if fi, err := os.Lstat(filepath.Join(p.Work, l.ns.Owner, l.ns.Repo, filepath.FromSlash(runner.Heartbeat))); err == nil {
			n.Heartbeat = fi.ModTime()
		}
		st.Namespaces = append(st.Namespaces, n)
	}
	p.mu.Unlock()
	st.Running = p.running()
	return st
}

// running lists the runs whose script still runs, in every attached namespace: "<ns> <agent>/<RUN>".
func (p *process) running() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for _, key := range slices.Sorted(maps.Keys(p.attached)) {
		for _, r := range p.attached[key].Runner.Running() {
			out = append(out, key+" "+r)
		}
	}
	return out
}

// ask sends req to the controller of work over its socket and decodes its one-line reply into v.
func ask(work string, req termRequest, v any) error {
	conn, err := net.DialTimeout("unix", filepath.Join(work, filepath.FromSlash(Socket)), 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("no reply: %w", err)
	}
	return json.Unmarshal(line, v)
}

// Query asks the controller of work for its Status.
func Query(work string) (Status, error) {
	var st Status
	err := ask(work, termRequest{Op: "status"}, &st)
	return st, err
}

// StopRefused is the controller's no to a stop without force: runs still run, which the stop would kill.
type StopRefused struct{ Reason string }

func (e *StopRefused) Error() string { return e.Reason }

// RequestStop asks the controller of work to stop; it returns once the controller has taken the request, not once it
// has stopped (Running says that). Without force a controller whose runs still run refuses (StopRefused); with it, it
// stops them, as on Ctrl-C.
func RequestStop(work string, force bool) error {
	var rep termReply
	if err := ask(work, termRequest{Op: "stop", Force: force}, &rep); err != nil {
		return err
	}
	if !rep.OK {
		return &StopRefused{rep.Reason}
	}
	return nil
}

// Refusal is the controller's no to a run in the terminal: the rule (when decide refused) and the reason.
type Refusal struct{ Rule, Reason string }

func (r *Refusal) Error() string {
	if r.Rule == "" {
		return r.Reason
	}
	return "denied by rule " + r.Rule + ": " + r.Reason
}

// TerminalRun is a run the controller granted to the user's terminal: Script runs, with Dir as its working directory
// and Lib as its $AAA_LIB.
type TerminalRun struct {
	Script, Sum, Dir, Lib string
	conn                  net.Conn
}

// StartTerminal asks the controller of work, over its socket, to start the run in this terminal. The caller holds the
// run's lock (runner.LockRun). A refusal is a *Refusal.
func StartTerminal(ctx context.Context, work string, ns names.Namespace, agent, run string) (*TerminalRun, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(work, filepath.FromSlash(Socket)))
	if err != nil {
		return nil, fmt.Errorf("the controller's socket: %w (is plinthctl controller running?)", err)
	}
	b, _ := json.Marshal(termRequest{Op: "exec", NS: ns.String(), Agent: agent, Run: run})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	var rep termReply
	if err == nil {
		err = json.Unmarshal(line, &rep)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("the controller's answer: %w", err)
	}
	if !rep.OK {
		conn.Close()
		return nil, &Refusal{Rule: rep.Rule, Reason: rep.Reason}
	}
	conn.SetReadDeadline(time.Time{})
	return &TerminalRun{Script: rep.Script, Sum: rep.Sum, Dir: rep.Dir, Lib: rep.Lib, conn: conn}, nil
}

// End tells the controller how the script ended (Wait's error), and closes the connection. An error means the controller
// didn't get it: it stopped meanwhile, and its next start finishes the run once the lock is free.
func (t *TerminalRun) End(waitErr error) error {
	defer t.conn.Close()
	b, _ := json.Marshal(termRequest{Op: "end", Result: runner.ExitText(waitErr)})
	t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := t.conn.Write(append(b, '\n'))
	return err
}

// Close drops the connection without a report, for a run that didn't start after all.
func (t *TerminalRun) Close() { t.conn.Close() }
