package plain

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"plinth/internal/doc"
)

//go:embed aaa.bash
var lib string

// Lib is the AAA library plinthctl ask lib prints, with bin, the binary itself, as the text filter it calls.
func Lib(bin string) []byte { return fill(bin, "0") }

// ControllerLib is the AAA library of a controller's run: the file $AAA_LIB names, with bin as its text filter
// (the controller's binary, as the run sees it). It leaves the checks of the run to the controller, which also writes
// the empty running.yml, and its last line says the controller scans the answer.
func ControllerLib(bin string) []byte { return fill(bin, "1") }

func fill(bin, plinth string) []byte {
	return []byte(strings.NewReplacer("@PLINTHCTL@", shellQuote(bin), "@PLINTH@", plinth).Replace(lib))
}

// TextArg is the hidden first argument of the AAA library's text filter; not in the help.
const TextArg = "__aaa-text"

// TextMain is plinthctl __aaa-text [-echo] -indent N <answer>: stdin appended to the answer file as the lines of a
// literal block at column N (doc.WriteText, in one write); -echo also copies stdin to stdout as it is read. The answer
// must be a regular file; a symlink is refused.
func TextMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(TextArg, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	echo := fs.Bool("echo", false, "copy stdin to stdout too")
	indent := fs.Int("indent", 0, "the column the block's lines start at")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *indent < 1 || *indent > 32 {
		fmt.Fprintln(stderr, "usage: plinthctl "+TextArg+" [-echo] -indent N <answer>")
		return 2
	}
	f, err := os.OpenFile(fs.Arg(0), os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", TextArg, err)
		return 1
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "plinthctl %s: %s: not a regular file\n", TextArg, fs.Arg(0))
		return 1
	}
	in := stdin
	if *echo {
		in = io.TeeReader(stdin, quiet{stdout})
	}
	if err := doc.WriteText(f, in, *indent); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", TextArg, err)
		return 1
	}
	return 0
}

// quiet is a writer whose errors don't count: the echo to a closed terminal mustn't lose the answer's text.
type quiet struct{ w io.Writer }

func (q quiet) Write(p []byte) (int, error) {
	q.w.Write(p)
	return len(p), nil
}
