package ollama

import (
	"errors"
	"fmt"
	"testing"
)

// TestKindOf: which errors of a call leave the model to be asked again later.
func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want Kind
	}{
		{ErrNotConfigured, CantAsk},
		{errTransient{fmt.Errorf("%w (http://o:11434 unreachable: connection refused)", ErrUnavailable)}, CantAsk},
		{errors.New("not available without Ollama (http://o:11434 unreachable: connection refused)"), CantAsk}, // through the gateway
		{errors.New("the Ollama gateway isn't running (plinthctl ollama-mcp start)"), CantAsk},
		{errors.New("the Ollama gateway: connection reset by peer"), CantAsk},
		{errors.New("Ollama: HTTP 502"), CantAsk},
		{errors.New("Ollama: HTTP 503: down"), CantAsk},
		{errors.New("Ollama: HTTP 504"), CantAsk},
		{errors.New(`Ollama: HTTP 404: model "dlp" not found, try pulling it first`), CantAsk},
		{errors.New("Ollama: no model"), CantAsk},
		{errors.New("Ollama: reading the reply: unexpected EOF"), CantAsk},
		{errors.New("Ollama: no reply within 5m0s (ollama.timeout-seconds)"), Timeout},
		{errors.New("Ollama: HTTP 500: out of memory"), Failed},
		{errors.New("Ollama: HTTP 400: bad request"), Failed},
		{errors.New("Ollama: HTTP 5030"), Failed},
		{errors.New("Ollama: the reply is larger than 16777216 bytes"), Failed},
		{errors.New("Ollama: empty reply"), Failed},
		{errors.New("the Ollama gateway: ollama_chat's result is not JSON: x"), Failed},
	} {
		if got := KindOf(c.err); got != c.want {
			t.Errorf("KindOf(%q) = %d, want %d", c.err, got, c.want)
		}
	}
}
