package runner

import (
	"fmt"
	"path/filepath"
	"strings"

	"plinth/internal/events"
	"plinth/internal/names"
)

// Waiting says what an exec's unscanned answer waits for, from the event log: the model, which can't be asked
// since a dlp.scan unavailable, or another attempt after scans that timed out. The last of those events says which:
// a timeout ends an outage, and one that follows begins a new one. Its zero value: the answer waits for
// nothing but a scan in progress (or it isn't unscanned).
type Waiting struct {
	Since    string // RFC 3339: the first dlp.scan unavailable since the exec's last timeout; "" while it can be asked
	Timeouts int    // the exec's dlp.scan timeout events
	Reason   string // the last of them: the error
}

// WaitingStatus is the answer status of an answer that waits for the model, for get ans and the TUI.
const WaitingStatus = "waiting-for-model"

// String is the line that says so: "waiting for the model: unavailable since <time> (<why>)", with "; <n> of
// <ScanAttempts> attempts timed out before" after timeouts, or "waiting for the model: <n> of <ScanAttempts> attempts
// timed out (<why>)"; "" for the zero value.
func (w Waiting) String() string {
	switch {
	case w.Since != "" && w.Timeouts > 0:
		return fmt.Sprintf("waiting for the model: unavailable since %s (%s); %d of %d attempts timed out before", w.Since, w.Reason, w.Timeouts, ScanAttempts)
	case w.Since != "":
		return fmt.Sprintf("waiting for the model: unavailable since %s (%s)", w.Since, w.Reason)
	case w.Timeouts > 0:
		return fmt.Sprintf("waiting for the model: %d of %d attempts timed out (%s)", w.Timeouts, ScanAttempts, w.Reason)
	}
	return ""
}

// ScanWaiting reads what run's unscanned answer waits for from the namespace's event log (Waiting). An exec.start
// begins it anew, a dlp.scan that ended the scan clears it.
func ScanWaiting(work string, ns names.Namespace, agent, run string) (Waiting, error) {
	var w Waiting
	key := agent + "/" + run
	err := events.Walk(work, filepath.Join(ns.Owner, ns.Repo), func(e events.Event, _ []byte) error {
		if e.Target != key {
			return nil
		}
		switch {
		case e.Action == "ask.stored" || e.Action == "exec.start":
			w = Waiting{}
		case e.Action == "dlp.scan" && e.Outcome == "unavailable":
			if w.Since == "" {
				w.Since = e.Time
			}
			w.Reason = waitReason(e.Reason)
		case e.Action == "dlp.scan" && e.Outcome == "timeout":
			w.Since = "" // the model was asked: the outage ended
			w.Timeouts++
			w.Reason = waitReason(e.Reason)
		case e.Action == "dlp.scan":
			w = Waiting{}
		}
		return nil
	})
	return w, err
}

// waitReason is the error of a dlp.scan unavailable or timeout event, without what scanStep adds to it.
func waitReason(reason string) string {
	for _, sep := range []string{"; the answer stays ", "; attempt "} {
		if before, _, ok := strings.Cut(reason, sep); ok {
			return before
		}
	}
	return reason
}
