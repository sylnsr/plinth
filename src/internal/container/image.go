package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"plinth/internal/gitrepo"
)

const (
	// RepoDir is the repo's directory that defines a namespace's image; Containerfile is the file in it.
	RepoDir       = ".plinth"
	Containerfile = "Containerfile"
	// ImageLabel marks a namespace's images; its value is the namespace, <owner>/<repo>.
	ImageLabel = "plinth.image"
	// MaxContext bounds the build context: the bytes of .plinth/'s files.
	MaxContext = 16 << 20
)

// buildTimeout bounds a build; it may install packages over the network.
var buildTimeout = 30 * time.Minute

// Build builds an image from files, the files of .plinth/ at one commit, with podman build's further args (labels,
// tags): the build context is those files alone, written by Plinth, and nothing Check refuses is built. It returns the
// image's ID, and the Containerfile's sha256 once it read one. A cancelled ctx ends the build; progress, when not nil,
// gets podman build's output as it comes.
func Build(ctx context.Context, files []gitrepo.File, progress io.Writer, args ...string) (id, sum string, err error) {
	dir, err := os.MkdirTemp("", "plinth-image-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	cdir := filepath.Join(dir, "context")
	if err := writeFiles(files, cdir); err != nil {
		return "", "", err
	}
	cf, err := os.ReadFile(filepath.Join(cdir, Containerfile))
	if err != nil {
		return "", "", fmt.Errorf("no %s", Containerfile)
	}
	s := sha256.Sum256(cf)
	sum = hex.EncodeToString(s[:])
	if refused := Refused(Check(files)); len(refused) > 0 {
		var msgs []string
		for _, f := range refused {
			msgs = append(msgs, f.String())
		}
		return "", sum, errors.New(strings.Join(msgs, "; "))
	}
	iid := filepath.Join(dir, "iid")
	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	// OCI format: it has no ONBUILD, so a build FROM this image (image publish's label) runs nothing, even where
	// BUILDAH_FORMAT=docker is set (devops review of a6e9ad6).
	cmd := exec.CommandContext(bctx, "podman", append(append([]string{"build", "--format", "oci", "--iidfile", iid, "--file",
		filepath.Join(cdir, Containerfile)}, args...), cdir)...)
	out := &tail{max: 4096}
	cmd.Stdout, cmd.Stderr = out, out
	if progress != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(out, progress), io.MultiWriter(out, progress)
	}
	if err := cmd.Run(); err != nil {
		return "", sum, fmt.Errorf("podman build: %v: %s", err, strings.TrimSpace(out.String()))
	}
	b, err := os.ReadFile(iid)
	if err != nil {
		return "", sum, err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "sha256:"), sum, nil
}

// writeFiles writes the files of .plinth/ into dir, the build context. gitrepo.Files gives regular files only, with
// paths inside the tree; each is still checked to land inside dir, and none may exist already.
func writeFiles(files []gitrepo.File, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return fmt.Errorf("%q: outside the directory", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if f.Exec {
			mode = 0o700
		}
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, err = w.Write(f.Data)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// tail keeps the last max bytes written to it: the end of a failed build says why.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
