package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"plinth/internal/names"
)

// TestSubmitted: a waiting Submitted gets the outcome as soon as the runner has it, once; one that never comes ends with
// the context; the runner keeps only the last maxSubmitted outcomes.
func TestSubmitted(t *testing.T) {
	l := New(context.Background(), &Services{}, names.Namespace{Owner: "michael", Repo: "devops"}, nil)
	got := make(chan Submitted, 1)
	go func() {
		s, _ := l.Submitted(context.Background(), "main", "0123456789abcdef")
		got <- s
	}()
	time.Sleep(50 * time.Millisecond)
	l.tookIn("ci", "0123456789abcdef", Submitted{Rule: "ask.lint", Reason: "another agent's"})
	l.tookIn("main", "0123456789abcdef", Submitted{Run: "calm-otter", Rule: "allow"})
	select {
	case s := <-got:
		if s != (Submitted{Run: "calm-otter", Rule: "allow"}) {
			t.Errorf("Submitted = %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Submitted didn't wake")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := l.Submitted(ctx, "main", "0123456789abcdef"); ok {
		t.Error("an outcome was given twice")
	}
	if s, ok := l.Submitted(context.Background(), "ci", "0123456789abcdef"); !ok || s.Rule != "ask.lint" {
		t.Errorf("the other agent's outcome: %+v %v", s, ok)
	}
	for i := range maxSubmitted + 1 {
		l.tookIn("main", fmt.Sprintf("%016x", i), Submitted{Run: "calm-otter"})
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, ok := l.Submitted(ctx2, "main", fmt.Sprintf("%016x", 0)); ok {
		t.Error("the oldest outcome was kept beyond maxSubmitted")
	}
	if _, ok := l.Submitted(context.Background(), "main", fmt.Sprintf("%016x", maxSubmitted)); !ok {
		t.Error("the newest outcome is gone")
	}
}

// TestRecord: nothing is recorded before the loop has opened the event log, nor once the runner is detached.
func TestRecord(t *testing.T) {
	l := New(context.Background(), &Services{}, names.Namespace{Owner: "michael", Repo: "devops"}, nil)
	if _, err := l.Record("main", "answer.read", "main/calm-otter", "allow", "allow", ""); err == nil || errors.Is(err, ErrDetached) {
		t.Errorf("before the log is open: %v", err)
	}
	l.Detached()
	if _, err := l.Record("main", "answer.read", "main/calm-otter", "allow", "allow", ""); !errors.Is(err, ErrDetached) {
		t.Errorf("detached: %v", err)
	}
	l.Kick()
	l.Kick() // a second one while one is due doesn't block
}
