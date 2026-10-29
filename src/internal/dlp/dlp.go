// Package dlp is the DLP scan's first step: masks for fixed patterns (private-key blocks, known token formats,
// password=-style assignments). They run before the model step and stay when that step fails or can't run, so a
// DLP-FAIL answer never shows a private key or a known token format. A mask replaces a value, never a line, so the
// rest of an error stays readable. Spec: plinthctl.README.md, "Needs Ollama" (dlp-scan) and "Process" (DLP scan).
package dlp

import (
	"bytes"
	"regexp"
)

// MaxSize is the largest text the DLP scan reads (16 MiB).
const MaxSize = 16 << 20

// mask is one fixed pattern and what replaces a match ($1 keeps a prefix such as "password=").
type mask struct {
	re   *regexp.Regexp
	with string
}

var (
	// keyBlock is a PEM private key; a block cut off before its END line is masked to the end of the text.
	keyBlock = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----(?s:.*?)(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|\z)`)
	masks    = []mask{
		{regexp.MustCompile(`plinth_[A-Za-z0-9_-]{43}`), "<plinth-token>"},
		{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "<jwt>"},
		{regexp.MustCompile(`AQ[A-Za-z0-9+/]{30,}={0,2}`), "<ceph-key>"},
		{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`), "<github-token>"},
		{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), "<gitlab-token>"},
		{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "<aws-key-id>"},
		{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), "<slack-token>"},
		{regexp.MustCompile(`\bsk-(ant-)?[A-Za-z0-9_-]{20,}`), "<api-key>"},
		{regexp.MustCompile(`(?i)\b((bearer|basic)\s+)[A-Za-z0-9._~+/=-]{12,}`), "$1<masked>"},
		// Forgejo's scheme, Authorization: token <token>; a bare "token" is masked only before a token's 40 hex
		// digits, since it is an ordinary word too.
		{regexp.MustCompile(`(?i)\b(authorization:[ \t]*token[ \t]+)[A-Za-z0-9._~+/=-]{8,}`), "$1<masked>"},
		{regexp.MustCompile(`(?i)\b(token[ \t]+)[0-9a-f]{40}\b`), "$1<masked>"},
		{regexp.MustCompile(`(://[^/:@\s]+:)[^@/\s]+@`), "$1<masked>@"},
		{regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(passw(or)?d|passphrase|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?key|client[_-]?key|credentials?)["']?[ \t]*[:=][ \t]*)("[^"\n]*"|'[^'\n]*'|[^"'\s,;}(][^\s,;}]*)`), "$1<masked>"},
	}
)

// Fixed masks every fixed pattern in b and returns the result and the number of masks put in. Running it again on its
// own result changes nothing more (a masked value is a placeholder, and a placeholder masks to itself).
func Fixed(b []byte) ([]byte, int) {
	n := 0
	out := keyBlock.ReplaceAllFunc(b, func([]byte) []byte {
		n++
		return []byte("<private key block masked>")
	})
	for _, m := range masks {
		out = m.re.ReplaceAllFunc(out, func(match []byte) []byte {
			masked := m.re.Expand(nil, []byte(m.with), match, m.re.FindSubmatchIndex(match))
			if !bytes.Equal(masked, match) {
				n++
			}
			return masked
		})
	}
	return out, n
}
