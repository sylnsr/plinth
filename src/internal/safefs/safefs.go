// Package safefs reads, writes and renames files below an os.Root without following any symlink, and makes every
// write and rename durable. os.Root alone keeps a path inside its directory but still follows a symlink that stays
// inside it; Plinth refuses those too. Spec: plinthctl.README.md, Principles ("No symlinks", "File names are the state").
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrSymlink: the path, or a directory on the way to it, is a symlink.
	ErrSymlink = errors.New("is a symlink")
	// ErrNotRegular: the path is not a regular file (a directory, device, FIFO, ...).
	ErrNotRegular = errors.New("is not a regular file")
	// ErrHardLinked: the file has more than one hard link, so another name (another run's file) can reach it.
	ErrHardLinked = errors.New("has more than one hard link")
)

// Lstat returns the FileInfo of name below root after checking that neither name nor any directory on the way to it
// is a symlink. name must be local: relative, without "..".
func Lstat(root *os.Root, name string) (fs.FileInfo, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("%s: not a local path", name)
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	var fi fs.FileInfo
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		var err error
		if fi, err = root.Lstat(p); err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", p, ErrSymlink)
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return nil, fmt.Errorf("%s: not a directory", p)
		}
	}
	return fi, nil
}

// Regular reports nil when name below root is a regular file with one hard link and no symlink on the way to it, the
// file OpenFile and ReadFile would open; otherwise the error says why.
func Regular(root *os.Root, name string) error {
	fi, err := Lstat(root, name)
	if err != nil {
		return err
	}
	return regular(name, fi)
}

// OpenFile opens the existing regular file name below root with flag. It refuses a symlink anywhere on the path, a
// file that isn't regular or has more than one hard link, and a file swapped between the check and the open (the
// opened file must be the one Lstat saw). It never creates a file: O_CREATE is refused.
func OpenFile(root *os.Root, name string, flag int) (*os.File, error) {
	if flag&os.O_CREATE != 0 {
		return nil, fmt.Errorf("%s: safefs.OpenFile does not create files", name)
	}
	fi, err := Lstat(root, name)
	if err != nil {
		return nil, err
	}
	if err := regular(name, fi); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, flag, 0)
	if err != nil {
		return nil, err
	}
	got, err := f.Stat()
	if err == nil && !os.SameFile(fi, got) {
		err = fmt.Errorf("%s: changed while it was opened", name)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadFile reads the regular file name below root (see OpenFile), at most limit bytes.
func ReadFile(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := OpenFile(root, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, limit)
	}
	return b, nil
}

// WriteNew writes dir/name below root so that name is either absent or complete, also after a crash, and never replaces
// an existing name, also when several processes create it at the same time: exactly one of them succeeds, the others
// get fs.ErrExist. It writes a temporary file with a random name (O_EXCL), syncs it, links it to name (a link fails if
// name exists), removes the temporary name and syncs dir. For a moment the new file has two links, so a concurrent
// reader that refuses hard-linked files may refuse it once. On a file system without hard links it checks that name
// is absent and renames instead. dir and every directory above it must be real directories.
func WriteNew(root *os.Root, dir, name string, body []byte, perm fs.FileMode) error {
	if err := isDir(root, dir); err != nil {
		return err
	}
	final := filepath.Join(dir, name)
	if _, err := root.Lstat(final); err == nil {
		return fmt.Errorf("%s: %w", final, fs.ErrExist)
	}
	tmp, err := tempFile(root, dir, name, body, perm)
	if err != nil {
		return err
	}
	err = root.Link(tmp, final)
	switch {
	case err == nil:
		err = root.Remove(tmp)
		if err == nil {
			err = syncDir(root, dir)
		}
		return err
	case errors.Is(err, fs.ErrExist):
		root.Remove(tmp)
		return fmt.Errorf("%s: %w", final, fs.ErrExist)
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.EMLINK):
		if _, err := root.Lstat(final); err == nil {
			root.Remove(tmp)
			return fmt.Errorf("%s: %w", final, fs.ErrExist)
		}
		if err := Rename(root, tmp, final); err != nil {
			root.Remove(tmp)
			return err
		}
		return nil
	default:
		root.Remove(tmp)
		return err
	}
}

// tempFile writes body to a new file in dir below root with a random name derived from name (O_EXCL, synced) and
// returns its path below root. On failure nothing is left behind.
func tempFile(root *os.Root, dir, name string, body []byte, perm fs.FileMode) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(rnd[:])+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return "", err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// Replace writes dir/name below root so that name holds either its old content or the complete new one, also after a
// crash and while others write it at the same time (the last rename wins): it writes a temporary file with a random
// name (O_EXCL), syncs it, renames it over name and syncs dir. dir and every directory above it must be real
// directories; an existing name must be a regular file (not a symlink, directory or hard-linked file).
func Replace(root *os.Root, dir, name string, body []byte, perm fs.FileMode) error {
	if err := isDir(root, dir); err != nil {
		return err
	}
	final := filepath.Join(dir, name)
	if fi, err := root.Lstat(final); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", final, ErrSymlink)
		}
		if err := regular(final, fi); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := tempFile(root, dir, name, body, perm)
	if err != nil {
		return err
	}
	if err := Rename(root, tmp, final); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

// Append appends body to dir/name below root with one write (O_APPEND), creating the file with perm if it is missing,
// so lines appended by several processes don't interleave. See OpenCreate for what it refuses. Not synced: for logs,
// not for state.
func Append(root *os.Root, dir, name string, body []byte, perm fs.FileMode) error {
	f, err := OpenCreate(root, dir, name, os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(body)
	return err
}

// OpenCreate opens dir/name below root with flag, creating it with perm if it is missing (O_CREATE is added). dir and
// every directory above it must be real directories; the file must be a regular file with one hard link, and it is
// checked again after the open, before the caller gets it, so a symlink swapped in meanwhile is refused. The file is
// never truncated by OpenCreate itself (O_TRUNC is refused): checks first, then changes.
func OpenCreate(root *os.Root, dir, name string, flag int, perm fs.FileMode) (*os.File, error) {
	p := filepath.Join(dir, name)
	if flag&os.O_TRUNC != 0 {
		return nil, fmt.Errorf("%s: safefs.OpenCreate does not truncate", p)
	}
	if err := isDir(root, dir); err != nil {
		return nil, err
	}
	if fi, err := root.Lstat(p); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", p, ErrSymlink)
		}
		if err := regular(p, fi); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(p, flag|os.O_CREATE, perm)
	if err != nil {
		return nil, err
	}
	got, err := f.Stat()
	if err == nil {
		if now, lerr := root.Lstat(p); lerr != nil || !os.SameFile(got, now) {
			err = fmt.Errorf("%s: changed while it was opened", p)
		} else {
			err = regular(p, got)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Touch sets the modification (and access) time of the existing regular file name below root to t, on the file it
// opened and checked (see OpenFile), never through a path a symlink could redirect.
func Touch(root *os.Root, name string, t time.Time) error {
	f, err := OpenFile(root, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{tv, tv})
}

// EnsureDir creates dir below root (mode 0700, its parent synced) unless it exists as a real directory, and says
// whether it created it. Its parent must exist; neither dir nor any directory on the way may be a symlink.
func EnsureDir(root *os.Root, dir string) (bool, error) {
	if err := isDir(root, filepath.Dir(dir)); err != nil {
		return false, err
	}
	err := root.Mkdir(dir, 0o700)
	if err == nil {
		return true, syncDir(root, filepath.Dir(dir))
	}
	if !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	return false, isDir(root, dir)
}

// ReadDir lists dir below root, which must be a real directory (no symlink on the way).
func ReadDir(root *os.Root, dir string) ([]fs.DirEntry, error) {
	if err := isDir(root, dir); err != nil {
		return nil, err
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadDir(-1)
}

// Remove deletes the regular file name below root (see OpenFile for what it refuses) and syncs its directory.
func Remove(root *os.Root, name string) error {
	fi, err := Lstat(root, name)
	if err != nil {
		return err
	}
	if err := regular(name, fi); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	return syncDir(root, filepath.Dir(name))
}

// Rename renames oldname to newname below root and then syncs the directories involved, so that once it returns the
// state change survives a crash. Neither oldname nor the directory of newname, nor any directory on the way, may be a
// symlink.
func Rename(root *os.Root, oldname, newname string) error {
	if _, err := Lstat(root, oldname); err != nil {
		return err
	}
	newDir, oldDir := filepath.Dir(newname), filepath.Dir(oldname)
	if err := isDir(root, newDir); err != nil {
		return err
	}
	if err := root.Rename(oldname, newname); err != nil {
		return err
	}
	if err := syncDir(root, newDir); err != nil {
		return err
	}
	if oldDir != newDir {
		return syncDir(root, oldDir)
	}
	return nil
}

func regular(name string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w", name, ErrNotRegular)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s: %w", name, ErrHardLinked)
	}
	return nil
}

func isDir(root *os.Root, dir string) error {
	fi, err := Lstat(root, dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	return nil
}

func syncDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
