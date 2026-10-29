package controller

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"plinth/internal/agentmcp"
	"plinth/internal/agents"
	"plinth/internal/decide"
	"plinth/internal/doc"
	"plinth/internal/mcp"
)

// TestAgentMCP: an agent works through the Plinth MCP server from start to end: the guide and the template, a
// submission the runner takes in at once, its states, and its answer once the user has released it; a wrong token is
// refused, and the decisions are in the event log.
func TestAgentMCP(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	tok, err := agents.Rotate(work, ns, "main")
	if err != nil {
		t.Fatal(err)
	}
	ln := mcpListener(t)
	start(t, Options{Work: work, MCPListener: ln})
	url := "http://" + ln.Addr().String() + agentmcp.Path
	c := &mcp.Client{HTTP: http.DefaultClient, URL: url, Token: tok}
	call := func(tool string, args any) string {
		t.Helper()
		out, err := c.Call(context.Background(), tool, args)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return out
	}

	eventually(t, "the namespace served", func() bool {
		_, err := c.Call(context.Background(), "guide", nil)
		return err == nil
	})
	if g := call("guide", nil); !strings.Contains(g, "## This namespace: michael/devops") || !strings.Contains(g, "`security` (automatic)") {
		t.Errorf("guide:\n%s", g)
	}
	var tmpl doc.AskTemplate
	if err := doc.Decode([]byte(call("ask_template", nil)), &tmpl); err != nil || tmpl.Spec.Mode != doc.ModePlinth {
		t.Fatalf("ask_template: %v %+v", err, tmpl)
	}
	bad := &mcp.Client{HTTP: http.DefaultClient, URL: url, Token: "plinth_" + strings.Repeat("x", 43)}
	var te *mcp.ToolError
	if _, err := bad.Call(context.Background(), "guide", nil); err == nil || errors.As(err, &te) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("a wrong token: %v", err)
	}

	script := string(filledTemplate(t)) + "say \"hello from an agent\"\n"
	out := call("ask_submit", map[string]any{"script": script})
	m := regexp.MustCompile(`^stored as ([a-z]+-[a-z]+) - next: `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("ask_submit: %q", out)
	}
	run := m[1]
	eventually(t, "the review failed (no model for reviews)", func() bool {
		return strings.Contains(call("ask_status", map[string]any{"run": run}), "ask: review-failed")
	})
	if out := call("answer_read", map[string]any{"run": run}); !strings.HasPrefix(out, "not released yet: "+run+"  ask: review-failed") {
		t.Errorf("answer_read before the run: %q", out)
	}

	// The user's part: override the failed review, run it, accept the flagged answer, release it.
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); len(f) != 0 {
		t.Fatalf("override and exec: %v", f)
	}
	eventually(t, "the answer scanned", func() bool { return load(t, work, run).AnswerStatus() == "dlp-failed" })
	if out := call("ask_status", nil); !strings.Contains(out, run+"  ask: ran  answer: dlp-failed - next: wait: the DLP scan flagged") {
		t.Errorf("ask_status after the scan: %q", out)
	}
	queue(t, work, run, decide.OverrideDLP)
	queue(t, work, run, decide.Done)
	if f := settled(t, work, run); len(f) != 0 {
		t.Fatalf("override dlp and done: %v", f)
	}
	answer := call("answer_read", map[string]any{"run": run})
	var a doc.Answer
	if err := doc.Decode([]byte(answer), &a); err != nil || len(a.Sections) == 0 || !strings.Contains(answer, "hello from an agent") {
		t.Fatalf("answer_read: %v\n%s", err, answer)
	}
	ev := strings.Join(eventList(t, work), "\n")
	for _, want := range []string{"main ask.submit allow allow", "main ask.stored ok allow", "main answer.read allow allow"} {
		if !strings.Contains(ev, want) {
			t.Errorf("no %q in the events:\n%s", want, ev)
		}
	}
	if !logLines(work, 1, `msg="the Plinth MCP server is listening"`, "url=http://"+ln.Addr().String())() {
		t.Error("the log doesn't say where the Plinth MCP server listens")
	}
}
