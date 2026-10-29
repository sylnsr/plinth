package plain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/review"
	"plinth/internal/safefs"
)

// ErrRefused: review refuses the arguments (exit 2): a type that doesn't exist, a file that is neither a script (.sh)
// nor an answer document (answer*.yml), or a file in done/.
var ErrRefused = errors.New("refused")

// Review is plinthctl review <type> <file>: it reviews an ask script (*.sh) or an answer (answer*.yml) with the prompt of
// reviewers.type.<type> and writes review.<type>.md, or review.answer.<type>.md for an answer, next to it; on any
// failure review[.answer].<type>.FAILED.md with the reason instead, and the error. The newest result replaces the
// slot's older one, passed or failed. A file in done/ (done/<RUN>/) has run: it is refused, and
// so is a type the config doesn't have (errors wrapping ErrRefused; nothing is written). It returns the path written.
func Review(ctx context.Context, c ollama.Chatter, model string, f *review.Fetcher, types map[string]config.ReviewType, typ, file string, now func() time.Time) (string, error) {
	t, ok := types[typ]
	if !ok {
		return "", fmt.Errorf("%w: review type %q is not in reviewers.type of the config", ErrRefused, typ)
	}
	kind, err := CheckReviewFile(file)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	dir, name := filepath.Split(abs)
	in := review.Input{Type: typ, Name: name, Kind: kind}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	write := func(failed bool, body []byte) error {
		if err := safefs.Replace(root, ".", in.File(failed), body, 0o600); err != nil {
			return err
		}
		if err := safefs.Remove(root, in.File(!failed)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	var p review.Prompt
	fail := func(why error) (string, error) {
		if err := write(true, review.Failed(in, why.Error(), p.Source, model, now())); err != nil {
			return "", fmt.Errorf("%v; writing %s: %w", why, in.File(true), err)
		}
		return filepath.Join(dir, in.File(true)), why
	}
	in.Body, err = safefs.ReadFile(root, name, 1<<20)
	if err != nil {
		return fail(err)
	}
	if p, err = f.Load(ctx, typ, t); err != nil {
		return fail(err)
	}
	res, err := review.Run(ctx, c, model, p, in, now)
	if err != nil {
		return fail(err)
	}
	if err := write(false, res.Markdown); err != nil {
		return "", err
	}
	return filepath.Join(dir, in.File(false)), nil
}

// CheckReviewFile is review's check of a file's name and place, before anything is read: an ask script (*.sh) or an
// answer (answer*.yml), not in done/ (errors wrapping ErrRefused). Shell completion marks the files it passes.
func CheckReviewFile(file string) (review.Kind, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return 0, err
	}
	dir, name := filepath.Split(abs)
	var kind review.Kind
	switch {
	case strings.HasSuffix(name, ".sh"):
		kind = review.Script
	case strings.HasPrefix(name, "answer.") && strings.HasSuffix(name, ".yml"):
		kind = review.Answer
	default:
		return 0, fmt.Errorf("%w: %s is neither an ask script (.sh) nor an answer (answer*.yml)", ErrRefused, file)
	}
	if d := filepath.Clean(dir); filepath.Base(d) == doneDir || filepath.Base(filepath.Dir(d)) == doneDir {
		return 0, fmt.Errorf("%w: %s is in done/: it has run already", ErrRefused, file)
	}
	return kind, nil
}
