package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// tree is a root directory with a/b/file.txt and an outside directory with secret.txt.
func tree(t *testing.T) (*os.Root, string, string) {
	t.Helper()
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "file.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir, outside
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	root, _, _ := tree(t)
	b, err := ReadFile(root, filepath.Join("a", "b", "file.txt"), 100)
	if err != nil || string(b) != "mine" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	if _, err := ReadFile(root, filepath.Join("a", "b", "file.txt"), 3); err == nil {
		t.Error("ReadFile ignored its limit")
	}
	for _, name := range []string{"../x", "/etc/passwd", ""} {
		if _, err := ReadFile(root, name, 100); err == nil {
			t.Errorf("ReadFile(%q) did not fail", name)
		}
	}
}

func TestRefusesSymlinks(t *testing.T) {
	root, dir, outside := tree(t)
	// A link to a file inside the root: os.Root alone would follow it.
	symlink(t, filepath.Join("b", "file.txt"), filepath.Join(dir, "a", "inside-file"))
	// A directory link inside the root, used as a path component.
	symlink(t, "b", filepath.Join(dir, "a", "inside-dir"))
	// Links out of the root.
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "outside-file"))
	symlink(t, outside, filepath.Join(dir, "outside-dir"))

	for _, name := range []string{
		filepath.Join("a", "inside-file"),
		filepath.Join("a", "inside-dir", "file.txt"),
		filepath.Join("a", "outside-file"),
		filepath.Join("outside-dir", "secret.txt"),
	} {
		if _, err := ReadFile(root, name, 100); !errors.Is(err, ErrSymlink) {
			t.Errorf("ReadFile(%s) = %v, want ErrSymlink", name, err)
		}
	}
}

func TestRefusesOtherFiles(t *testing.T) {
	root, dir, _ := tree(t)
	if _, err := ReadFile(root, "a", 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("a directory: %v, want ErrNotRegular", err)
	}
	if err := os.Link(filepath.Join(dir, "a", "b", "file.txt"), filepath.Join(dir, "a", "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, filepath.Join("a", "hard.txt"), 100); !errors.Is(err, ErrHardLinked) {
		t.Errorf("a hard link: %v, want ErrHardLinked", err)
	}
	if _, err := OpenFile(root, filepath.Join("a", "new.txt"), os.O_RDWR|os.O_CREATE); err == nil {
		t.Error("OpenFile created a file")
	}
}

func TestWriteNew(t *testing.T) {
	root, dir, _ := tree(t)
	if err := WriteNew(root, "a", "ask.sh", []byte("#!/bin/bash\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "a", "ask.sh"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("ask.sh: %v, %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "a", ".ask.sh.*")); len(left) > 0 {
		t.Errorf("temporary file left behind: %v", left)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		t.Errorf("ask.sh has %d links, want 1", st.Nlink)
	}
	if err := WriteNew(root, "a", "ask.sh", []byte("again"), 0o700); !errors.Is(err, fs.ErrExist) {
		t.Errorf("WriteNew over an existing file: %v, want ErrExist", err)
	}
	symlink(t, "b", filepath.Join(dir, "a", "link"))
	if err := WriteNew(root, filepath.Join("a", "link"), "x", nil, 0o600); !errors.Is(err, ErrSymlink) {
		t.Errorf("WriteNew into a symlinked directory: %v, want ErrSymlink", err)
	}
}

// Several processes creating the same file at once: exactly one wins, the others get ErrExist, nothing is left behind.
func TestWriteNewConcurrent(t *testing.T) {
	root, dir, _ := tree(t)
	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WriteNew(root, "a", "config.yml", []byte(strconv.Itoa(i)), 0o600)
		}()
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, fs.ErrExist):
			t.Errorf("a loser got %v, want ErrExist", err)
		}
	}
	if won != 1 {
		t.Errorf("%d writers won, want exactly 1", won)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "a", ".config.yml.*")); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestRename(t *testing.T) {
	root, dir, _ := tree(t)
	if err := os.Mkdir(filepath.Join(dir, "failed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Rename(root, "a", filepath.Join("failed", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "a", "b", "file.txt")); err != nil {
		t.Errorf("not moved: %v", err)
	}
	symlink(t, "failed", filepath.Join(dir, "elsewhere"))
	if err := Rename(root, filepath.Join("failed", "a"), filepath.Join("elsewhere", "a2")); !errors.Is(err, ErrSymlink) {
		t.Errorf("Rename into a symlinked directory: %v, want ErrSymlink", err)
	}
	if err := Rename(root, "elsewhere", "moved"); !errors.Is(err, ErrSymlink) {
		t.Errorf("Rename of a symlink: %v, want ErrSymlink", err)
	}
}

func TestReplace(t *testing.T) {
	root, dir, outside := tree(t)
	p := filepath.Join(dir, "a", "agent.sha256")
	for _, body := range []string{"first\n", "second\n"} {
		if err := Replace(root, "a", "agent.sha256", []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != body {
			t.Fatalf("after Replace: %q, %v; want %q", b, err, body)
		}
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "a", ".*.tmp")); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "link.sha256"))
	if err := Replace(root, "a", "link.sha256", []byte("x"), 0o600); !errors.Is(err, ErrSymlink) {
		t.Errorf("Replace over a symlink: %v, want ErrSymlink", err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "theirs" {
		t.Errorf("the symlink's target was written: %q", b)
	}
	if err := Replace(root, ".", "a", []byte("x"), 0o600); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Replace over a directory: %v, want ErrNotRegular", err)
	}
	symlink(t, "b", filepath.Join(dir, "a", "dirlink"))
	if err := Replace(root, filepath.Join("a", "dirlink"), "x", nil, 0o600); !errors.Is(err, ErrSymlink) {
		t.Errorf("Replace into a symlinked directory: %v, want ErrSymlink", err)
	}
}

func TestRemove(t *testing.T) {
	root, dir, outside := tree(t)
	if err := Remove(root, filepath.Join("a", "b", "file.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a", "b", "file.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("not removed: %v", err)
	}
	if err := Remove(root, filepath.Join("a", "b", "file.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Remove of a missing file: %v, want ErrNotExist", err)
	}
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "link"))
	if err := Remove(root, filepath.Join("a", "link")); !errors.Is(err, ErrSymlink) {
		t.Errorf("Remove of a symlink: %v, want ErrSymlink", err)
	}
	if err := Remove(root, "a"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Remove of a directory: %v, want ErrNotRegular", err)
	}
}

func TestAppend(t *testing.T) {
	root, dir, outside := tree(t)
	for _, line := range []string{"one\n", "two\n"} {
		if err := Append(root, "a", "x.log", []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a", "x.log")); string(b) != "one\ntwo\n" {
		t.Errorf("x.log = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "a", "x.log")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	symlink(t, filepath.Join("b", "file.txt"), filepath.Join(dir, "a", "inside.log"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "outside.log"))
	for _, name := range []string{"inside.log", "outside.log"} {
		if err := Append(root, "a", name, []byte("x\n"), 0o600); !errors.Is(err, ErrSymlink) {
			t.Errorf("Append to %s: %v, want ErrSymlink", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a", "b", "file.txt")); string(b) != "mine" {
		t.Errorf("a symlink's target was written: %q", b)
	}
	if err := os.Link(filepath.Join(dir, "a", "x.log"), filepath.Join(dir, "a", "hard.log")); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, "a", "hard.log", []byte("x\n"), 0o600); !errors.Is(err, ErrHardLinked) {
		t.Errorf("Append to a hard link: %v, want ErrHardLinked", err)
	}
}

func TestOpenCreate(t *testing.T) {
	root, dir, outside := tree(t)
	f, err := OpenCreate(root, "a", "x.lock", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("123")
	f.Close()
	// An existing file is opened as it is, not truncated.
	if f, err = OpenCreate(root, "a", "x.lock", os.O_RDWR, 0o600); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "a", "x.lock")); string(b) != "123" {
		t.Errorf("content = %q", b)
	}
	if _, err := OpenCreate(root, "a", "x.lock", os.O_RDWR|os.O_TRUNC, 0o600); err == nil {
		t.Error("OpenCreate took O_TRUNC")
	}
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "out.lock"))
	if _, err := OpenCreate(root, "a", "out.lock", os.O_RDWR, 0o600); !errors.Is(err, ErrSymlink) {
		t.Errorf("OpenCreate of a symlink: %v, want ErrSymlink", err)
	}
	symlink(t, "a", filepath.Join(dir, "link"))
	if _, err := OpenCreate(root, "link", "y.lock", os.O_RDWR, 0o600); !errors.Is(err, ErrSymlink) {
		t.Errorf("OpenCreate below a symlinked directory: %v, want ErrSymlink", err)
	}
}

func TestTouch(t *testing.T) {
	root, dir, outside := tree(t)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := Touch(root, filepath.Join("a", "b", "file.txt"), when); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a", "b", "file.txt")); err != nil || !fi.ModTime().Equal(when) {
		t.Errorf("mtime = %v, %v; want %v", fi.ModTime(), err, when)
	}
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a", "beat"))
	if err := Touch(root, filepath.Join("a", "beat"), when); !errors.Is(err, ErrSymlink) {
		t.Errorf("Touch of a symlink: %v, want ErrSymlink", err)
	}
	if fi, _ := os.Stat(filepath.Join(outside, "secret.txt")); fi.ModTime().Equal(when) {
		t.Error("Touch followed a symlink out of the root")
	}
}

func TestEnsureDir(t *testing.T) {
	root, dir, outside := tree(t)
	if created, err := EnsureDir(root, filepath.Join("a", "c")); err != nil || !created {
		t.Errorf("new dir: %v, %v", created, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "a", "c")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("a/c: %v, %v", fi, err)
	}
	if created, err := EnsureDir(root, filepath.Join("a", "c")); err != nil || created {
		t.Errorf("existing dir: %v, %v", created, err)
	}
	if _, err := EnsureDir(root, filepath.Join("a", "b", "file.txt")); err == nil {
		t.Error("a file passed as a directory")
	}
	if _, err := EnsureDir(root, filepath.Join("x", "y")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing parent: %v", err)
	}
	symlink(t, outside, filepath.Join(dir, "a", "out"))
	if _, err := EnsureDir(root, filepath.Join("a", "out")); !errors.Is(err, ErrSymlink) {
		t.Errorf("symlinked dir: %v, want ErrSymlink", err)
	}
	if _, err := EnsureDir(root, filepath.Join("a", "out", "new")); !errors.Is(err, ErrSymlink) {
		t.Errorf("below a symlink: %v, want ErrSymlink", err)
	}
}

func TestReadDir(t *testing.T) {
	root, dir, outside := tree(t)
	if entries, err := ReadDir(root, "a"); err != nil || len(entries) != 1 || entries[0].Name() != "b" {
		t.Errorf("ReadDir(a) = %v, %v", entries, err)
	}
	if _, err := ReadDir(root, filepath.Join("a", "b", "file.txt")); err == nil {
		t.Error("a file listed as a directory")
	}
	if _, err := ReadDir(root, "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dir: %v", err)
	}
	symlink(t, outside, filepath.Join(dir, "a", "out"))
	if _, err := ReadDir(root, filepath.Join("a", "out")); !errors.Is(err, ErrSymlink) {
		t.Errorf("symlinked dir: %v, want ErrSymlink", err)
	}
}
