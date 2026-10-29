package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"plinth/internal/gitrepo"
	"plinth/internal/names"
)

// namespaceArg is the namespace -n names; without -n, the one named by the origin remote of the git checkout the
// current directory is in. That origin must be on forgejo-url's host, the namespace must be one <work> has, and the
// command says on stderr which namespace it took: the checkout's .git/config may be an agent's to write, so it never
// picks a namespace silently. work is expanded already.
func namespaceArg(c *command, n, work string, stderr io.Writer) (names.Namespace, int, bool) {
	if n != "" {
		ns, err := names.ParseNamespace(n)
		if err != nil {
			return names.Namespace{}, usageError(c, stderr, err.Error()), false
		}
		return ns, exitOK, true
	}
	fail := func(err error) (names.Namespace, int, bool) {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return names.Namespace{}, exitFail, false
	}
	top, err := gitrepo.TopLevel(".")
	if err != nil {
		return names.Namespace{}, usageError(c, stderr, "-n <owner>/<repo> is required outside a checkout of the repo"), false
	}
	ns, err := namespaceOf("", top)
	if err != nil {
		return fail(err)
	}
	note, err := checkOrigin("", top, work)
	switch {
	case err != nil:
		return fail(err)
	case note != "":
		return fail(errors.New(note + "; give -n <owner>/<repo>"))
	}
	if fi, err := os.Stat(filepath.Join(work, ns.Owner, ns.Repo)); err != nil || !fi.IsDir() {
		return fail(fmt.Errorf("%s, from the origin of %s, is not a namespace in %s (plinthctl register sets one up); give -n <owner>/<repo>",
			ns, top, work))
	}
	fmt.Fprintf(stderr, "plinthctl %s: namespace %s (the origin of %s)\n", c.name, ns, top)
	return ns, exitOK, true
}
