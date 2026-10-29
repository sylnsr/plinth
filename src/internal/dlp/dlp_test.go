package dlp

import (
	"strings"
	"testing"
)

func TestFixed(t *testing.T) {
	token := "plinth_" + strings.Repeat("aB3_-", 8) + "xyz"
	for _, c := range []struct {
		name, in, want string
	}{
		{"private key", "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\nAAAA\n-----END OPENSSH PRIVATE KEY-----\nafter\n",
			"before\n<private key block masked>\nafter\n"},
		{"cut-off private key", "x\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n", "x\n<private key block masked>"},
		{"plinth token", "PLINTH_TOKEN " + token + " end", "PLINTH_TOKEN <plinth-token> end"},
		{"jwt", "auth eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4", "auth <jwt>"},
		{"ceph key", "key = AQBvaBFZAAAAABAA9VHgwCNm1Yk2EXdOECwvlA==", "key = <ceph-key>"},
		{"github token", "ghp_" + strings.Repeat("a1", 18), "<github-token>"},
		{"aws key id", "id AKIAIOSFODNN7EXAMPLE.", "id <aws-key-id>."},
		{"bearer", "Authorization: Bearer abcdef0123456789xyz", "Authorization: Bearer <masked>"},
		{"forgejo token header", `curl -H "Authorization: token 0123456789abcdef0123456789abcdef01234567" url`, `curl -H "Authorization: token <masked>" url`},
		{"forgejo token", "using token 0123456789abcdef0123456789abcdef01234567.", "using token <masked>."},
		{"token in prose", "the token expired at 10:00, renew the token", "the token expired at 10:00, renew the token"},
		{"url password", "git clone https://bob:hunter2@forgejo.example.org/x.git", "git clone https://bob:<masked>@forgejo.example.org/x.git"},
		{"password assignment", `db_password="s3cret pass" user=bob`, `db_password=<masked> user=bob`},
		{"api key yaml", "  api-key: 0123abcd, next", "  api-key: <masked>, next"},
		{"nothing secret", "ok: 3 disks, sda sdb sdc\n", "ok: 3 disks, sda sdb sdc\n"},
	} {
		got, n := Fixed([]byte(c.in))
		if string(got) != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if (n == 0) != (c.in == c.want) {
			t.Errorf("%s: %d mask(s)", c.name, n)
		}
		// Masking again changes nothing.
		if again, n2 := Fixed(got); string(again) != string(got) || n2 != 0 {
			t.Errorf("%s: masking again gave %q (%d)", c.name, again, n2)
		}
	}
}
