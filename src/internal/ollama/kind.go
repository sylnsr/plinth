package ollama

import (
	"errors"
	"regexp"
	"strings"
)

// Kind is how a failed call to Ollama ended, for a caller that may ask again: Failed, CantAsk or Timeout.
type Kind int

const (
	// Failed: Ollama took the request and answered with an error (an HTTP status not in CantAsk, a reply that is
	// too large, or that isn't what was asked for): asking again gives the same.
	Failed Kind = iota
	// CantAsk: the model can't be asked: Ollama not configured or unreachable, the Ollama gateway not running or not
	// answering, the model missing, or HTTP 502, 503 or 504 (Ollama loading a model, a proxy in front of it). Ask
	// again later.
	CantAsk
	// Timeout: no reply within ollama.timeout-seconds.
	Timeout
)

// canAskRe are the texts of CantAsk: the errors of the gateway's client come back as text, so the kind is read
// from it (ollamamcp.Client: "the Ollama gateway isn't running", "the Ollama gateway: <transport error>").
var canAskRe = regexp.MustCompile(`not available without Ollama|the Ollama gateway isn't running|^the Ollama gateway: |` +
	`Ollama: HTTP 50[234]\b|Ollama: HTTP 404: .*not found|Ollama: no model$|Ollama: reading the reply: `)

// KindOf is the kind of err, an error of a call to Ollama, in this process or through the gateway.
func KindOf(err error) Kind {
	if err == nil {
		return Failed
	}
	if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrUnavailable) {
		return CantAsk
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "Ollama: no reply within "):
		return Timeout
	case strings.Contains(s, "result is not JSON"): // the gateway answered, with something else
		return Failed
	case canAskRe.MatchString(s):
		return CantAsk
	}
	return Failed
}
