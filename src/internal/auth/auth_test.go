package auth

import "testing"

func TestCallers(t *testing.T) {
	u, a, zero := HostUser(), Submission("main"), Caller{}
	if !u.IsUser() || u.IsAgent() || u.Agent() != "" || u.String() != "user" {
		t.Errorf("HostUser = %#v", u)
	}
	if a.IsUser() || !a.IsAgent() || a.Agent() != "main" || a.String() != "main" {
		t.Errorf("Submission(main) = %#v", a)
	}
	// An agent named user is still an agent: the kind comes from the channel, never from the name.
	if n := Submission("user"); n.IsUser() || !n.IsAgent() {
		t.Errorf("Submission(user) = %#v", n)
	}
	if zero.IsUser() || zero.IsAgent() || zero.String() != "?" {
		t.Errorf("zero Caller = %#v", zero)
	}
}
