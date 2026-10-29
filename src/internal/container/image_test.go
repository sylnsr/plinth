package container

import (
	"os"
	"path/filepath"
	"testing"

	"plinth/internal/gitrepo"
)

const pin = "@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89"

func TestWriteFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	files := []gitrepo.File{{Path: "Containerfile", Data: []byte("FROM x\n")}, {Path: "sub/run.sh", Exec: true, Data: []byte("x")}}
	if err := writeFiles(files, dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "sub", "run.sh")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("sub/run.sh: %v, %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Containerfile")); err != nil || string(b) != "FROM x\n" {
		t.Errorf("Containerfile: %q, %v", b, err)
	}
	for what, f := range map[string][]gitrepo.File{
		"..":    {{Path: "../x"}},
		"twice": {{Path: "a"}, {Path: "a"}},
	} {
		if err := writeFiles(f, filepath.Join(t.TempDir(), "c")); err == nil {
			t.Errorf("%s: written", what)
		}
	}
}
