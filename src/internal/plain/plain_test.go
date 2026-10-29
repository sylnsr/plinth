package plain

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

const tplBody = "#!/bin/bash\nRUN_NAME=\"<adjective>-<noun>-<issue>\"   # comment stays\necho hi\n"

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// workDir is an empty work directory: new-ask's default template is embedded.
func workDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestTakenStems(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"calm-otter", "done/brave-fox-12", "failed/merry-moss", "main/done/swift-yak-3", "handoff/keen-owl", "a/b/c/deep-elm"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "quiet-tide"), "")
	write(t, filepath.Join(dir, "ask-bold-wren-7.sh"), "")
	write(t, filepath.Join(dir, "done", "answer-noble-pine.FAIL.md"), "")
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "brave-bison"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "lucky-lark")); err != nil {
		t.Fatal(err)
	}
	taken, err := TakenStems(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"calm-otter", "brave-fox", "merry-moss", "swift-yak", "keen-owl", "quiet-tide", "lucky-lark"} {
		if !taken[s] {
			t.Errorf("%s not taken", s)
		}
	}
	for s, why := range map[string]string{
		"deep-elm":    "is four levels down",
		"bold-wren":   "is only a legacy flat file",
		"noble-pine":  "is only a legacy flat file",
		"brave-bison": "is behind a symlink",
	} {
		if taken[s] {
			t.Errorf("%s taken, but %s", s, why)
		}
	}
	if _, err := TakenStems(filepath.Join(dir, "missing")); err == nil {
		t.Error("TakenStems on a missing directory did not fail")
	}
}

func TestRunName(t *testing.T) {
	dir := t.TempDir()
	run, err := RunName(dir, "42")
	if err != nil || !names.ValidRun(run) || !strings.HasSuffix(run, "-42") {
		t.Fatalf("RunName = %q, %v", run, err)
	}
	if _, err := RunName(dir, "4x"); err == nil {
		t.Error("RunName accepted a bad issue number")
	}
}

func TestNewAsk(t *testing.T) {
	dir := workDir(t)
	out, err := NewAsk(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Base(filepath.Dir(out))
	if !names.ValidRun(run) || out != filepath.Join(dir, run, "ask.sh") {
		t.Fatalf("NewAsk wrote %s", out)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "RUN_NAME=\""+run+"\"   # filled in by plinthctl new-ask") || !strings.HasPrefix(string(b), string(workdir.AAATemplate[:40])) {
		t.Errorf("RUN_NAME not filled in, or not the embedded template:\n%s", b)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, run)); len(entries) != 1 {
		t.Errorf("run directory holds %d entries, want only ask.sh", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); err != nil {
		t.Errorf("no lock file: %v", err)
	}
	// A second ask never reuses the stem.
	out2, err := NewAsk(dir, "", "", "")
	if err != nil || names.Stem(filepath.Base(filepath.Dir(out2))) == names.Stem(run) {
		t.Errorf("second NewAsk = %s, %v", out2, err)
	}

	// A template the user names is read as given.
	other := filepath.Join(t.TempDir(), "other.template")
	write(t, other, tplBody)
	if _, err := NewAsk(dir, "", other, "7"); err != nil {
		t.Errorf("NewAsk with --template: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.template")
	write(t, bad, "#!/bin/bash\necho no name line\n")
	if _, err := NewAsk(dir, "", bad, ""); err == nil {
		t.Error("NewAsk accepted a template without a RUN_NAME line")
	}

	// A manifest: of mode aaa its script, of mode plinth refused; <work>/ask-template.yml without --template, a
	// broken one refused, none the embedded base.
	work := t.TempDir()
	mine := doc.AskTemplate{APIVersion: doc.APIVersion, Kind: doc.KindAskTemplate, Metadata: doc.TemplateMeta{Name: "mine"},
		Spec: doc.AskTemplateSpec{Mode: doc.ModeAAA, Script: tplBody}}
	write(t, filepath.Join(work, workdir.BaseTemplateName), string(mine.Encode()))
	if out, err := NewAsk(dir, work, "", ""); err != nil {
		t.Errorf("NewAsk from <work>: %v", err)
	} else if b, _ := os.ReadFile(out); !strings.HasSuffix(string(b), "# comment stays\necho hi\n") {
		t.Errorf("NewAsk from <work> wrote:\n%s", b)
	}
	mine.Spec.Mode = doc.ModePlinth
	write(t, other, string(mine.Encode()))
	if _, err := NewAsk(dir, work, other, ""); err == nil || !strings.Contains(err.Error(), "mode plinth") {
		t.Errorf("NewAsk with a plinth manifest: %v", err)
	}
	write(t, filepath.Join(work, workdir.BaseTemplateName), "kind: AskTemplate\n")
	if _, err := NewAsk(dir, work, "", ""); err == nil || !strings.Contains(err.Error(), workdir.BaseTemplateName) {
		t.Errorf("NewAsk with a broken <work> template: %v", err)
	}
	if out, err := NewAsk(dir, t.TempDir(), "", ""); err != nil {
		t.Errorf("NewAsk with a <work> without a template: %v", err)
	} else if b, _ := os.ReadFile(out); !strings.HasPrefix(string(b), string(workdir.AAATemplate[:40])) {
		t.Errorf("not the embedded base:\n%s", b)
	}
}

func TestNewAskLockSymlink(t *testing.T) {
	dir := workDir(t)
	write(t, filepath.Join(dir, "elsewhere"), "")
	if err := os.Symlink("elsewhere", filepath.Join(dir, lockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAsk(dir, "", "", ""); !errors.Is(err, safefs.ErrSymlink) {
		t.Errorf("NewAsk = %v, want ErrSymlink", err)
	}
}

func TestNewAskLockTimeout(t *testing.T) {
	dir := workDir(t)
	if _, err := NewAsk(dir, "", "", ""); err != nil { // creates the lock file
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = 10 * time.Second })

	start := time.Now()
	_, err = NewAsk(dir, "", "", "")
	if err == nil || !strings.Contains(err.Error(), lockFile) {
		t.Fatalf("NewAsk with the lock held = %v, want an error naming %s", err, lockFile)
	}
	if waited := time.Since(start); waited < lockWait || waited > 5*time.Second {
		t.Errorf("waited %v, want about %v", waited, lockWait)
	}
}

func TestNewAskConcurrent(t *testing.T) {
	dir := workDir(t)
	const n = 16
	outs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = NewAsk(dir, "", "", strconv.Itoa(i))
		}()
	}
	wg.Wait()
	stems := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("NewAsk %d: %v", i, errs[i])
		}
		s := names.Stem(filepath.Base(filepath.Dir(outs[i])))
		if stems[s] {
			t.Errorf("stem %s drawn twice", s)
		}
		stems[s] = true
	}
}

func TestNewAskFailedMovesRun(t *testing.T) {
	dir := workDir(t)
	writeAsk = func(*os.Root, string, string, []byte, os.FileMode) error { return errors.New("disk full") }
	t.Cleanup(func() { writeAsk = safefs.WriteNew })

	if _, err := NewAsk(dir, "", "", "3"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("NewAsk = %v, want the write error", err)
	}
	failed, err := os.ReadDir(filepath.Join(dir, failedDir))
	if err != nil || len(failed) != 1 || !names.ValidRun(failed[0].Name()) || !strings.HasSuffix(failed[0].Name(), "-3") {
		t.Fatalf("failed/ = %v, %v", failed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, failed[0].Name())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run directory is still in place: %v", err)
	}
	taken, _ := TakenStems(dir)
	if !taken[names.Stem(failed[0].Name())] {
		t.Error("a failed run's name is free again")
	}
}

// TestOpenWorkDir: a missing ai/work is fs.ErrNotExist without create and made with it; a symlinked ai or
// ai/work is refused either way, and nothing is made through it.
func TestOpenWorkDir(t *testing.T) {
	repo := t.TempDir()
	if _, err := OpenWorkDir(repo, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	root, err := OpenWorkDir(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if fi, err := os.Lstat(WorkDir(repo)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("created: %v, %v", fi, err)
	}
	if root, err = OpenWorkDir(repo, false); err != nil {
		t.Errorf("existing: %v", err)
	} else {
		root.Close()
	}
	outside := t.TempDir()
	for _, link := range []string{"ai", filepath.Join("ai", "work")} {
		r := t.TempDir()
		if err := os.MkdirAll(filepath.Join(r, filepath.Dir(link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(r, link)); err != nil {
			t.Fatal(err)
		}
		for _, create := range []bool{false, true} {
			if _, err := OpenWorkDir(r, create); !errors.Is(err, safefs.ErrSymlink) {
				t.Errorf("%s a symlink, create %v: %v", link, create, err)
			}
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("made through the symlink: %v", entries)
	}
}
