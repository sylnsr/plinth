package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/forgejo"
	"plinth/internal/image"
	"plinth/internal/imagetest"
	"plinth/internal/names"
)

// TestImageCommands: list shows the registry's images with the pin and this machine's copies marked; use pins another
// image of the namespace after the user types use; delete never touches the pinned image, removes this machine's copy
// with --local, and deletes from the registry with --registry only after the user types delete; pull gets the pin.
// The images are published through package image, against the stand-in for Forgejo.
func TestImageCommands(t *testing.T) {
	img := imagetest.NeedTools(t)
	ns := names.Namespace{Owner: "michael", Repo: "cli-image-test-" + strconv.Itoa(os.Getpid())}
	f := imagetest.NewForge(t, ns, "plinth-token")
	t.Cleanup(func() { imagetest.RemoveImages(ns) })
	f.Trust(t)
	f.Login(t)
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, f.URL)
	cfgFile, err := os.OpenFile(filepath.Join(work, "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfgFile.WriteString("forgejo-token: plinth-token\n")
	cfgFile.Close()
	nsDir := filepath.Join(work, ns.Owner, ns.Repo)
	for _, d := range []string{nsDir, filepath.Join(work, "logs")} { // as register sets them up: the log needs logs/
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	g, err := config.LoadGlobal(work, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	pinOf := func() config.ImagePin {
		n, _, err := config.LoadNamespace(work, ns, g, os.LookupEnv)
		if err != nil {
			t.Fatal(err)
		}
		return n.Image
	}

	// Two images, published as image publish does; the second is pinned.
	fj, err := forgejo.New(f.URL, f.Token, f.CA)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(containerfile string) image.Published {
		t.Helper()
		f.Commit(t, containerfile, "an image")
		plan, err := image.Prepare(context.Background(), fj, nsDir, ns, pinOf(), "")
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Close()
		pub, err := plan.Publish(context.Background(), time.Now(), nil, func(p config.ImagePin) error {
			return config.SetImage(work, ns, g, os.LookupEnv, p)
		})
		if err != nil {
			t.Fatal(err)
		}
		return pub
	}
	one := publish("FROM " + img + "\nRUN echo one > /built.txt\n")
	two := publish("FROM " + img + "\nRUN echo two > /built.txt\n")
	if pinOf() != two.Pin {
		t.Fatalf("pin %+v, want %+v", pinOf(), two.Pin)
	}

	nsArgs := []string{"-n", ns.String(), "--work", work}
	cmd := func(answer string, args ...string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		last := len(args) - 1 // flags before the argument: flag parsing stops at the first one
		return run(append(append(append([]string{"image"}, args[:last]...), nsArgs...), args[last])...)
	}
	lineOf := func(out, tag string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, tag) {
				return l
			}
		}
		return ""
	}

	// list: both, newest first; the pin and this machine's copies marked.
	code, out, errs := run(append([]string{"image", "list"}, nsArgs...)...)
	if code != exitOK || !strings.Contains(out, "Pinned:     "+two.Pin.Ref) || strings.Index(out, two.Tag) > strings.Index(out, one.Tag) ||
		!strings.Contains(lineOf(out, two.Tag), "pinned, here") || strings.Contains(lineOf(out, one.Tag), "pinned") {
		t.Errorf("image list: %d %q\n%s", code, errs, out)
	}
	code, out, _ = run(append([]string{"image", "list", "-o", "yaml"}, nsArgs...)...)
	if code != exitOK || !strings.Contains(out, two.Pin.Ref) || !strings.Contains(out, "tag: "+one.Tag) || !strings.Contains(out, "pinned: true") {
		t.Errorf("image list -o yaml: %d\n%s", code, out)
	}

	// use: the first image again, by its tag, once the user types use.
	if code, out, _ := cmd("no\n", "use", one.Tag); code != exitFail || !strings.Contains(out, "Nothing changed.") || pinOf() != two.Pin {
		t.Errorf("use, not confirmed: %d\n%s", code, out)
	}
	code, out, errs = cmd("yes\n", "use", one.Tag)
	if code != exitOK || pinOf() != one.Pin || !strings.Contains(out, "Commit:   "+one.Pin.Commit) || !strings.Contains(out, "another .plinth/ tree") {
		t.Errorf("use: %d %q\n%s", code, errs, out)
	}
	if code, out, _ := cmd("", "use", one.Pin.Digest()); code != exitOK || !strings.Contains(out, "is the pin of "+ns.String()+" already") {
		t.Errorf("use of the pin: %d\n%s", code, out)
	}

	// delete: never the pin; --local removes this machine's copy; --registry only after delete is typed.
	if code, _, errs := cmd("yes\n", "delete", "--registry", one.Tag); code != exitFail || !strings.Contains(errs, "is pinned: use another image first") {
		t.Errorf("delete of the pin: %d %q", code, errs)
	}
	if code, _, errs := cmd("", "delete", "--local", one.Pin.Digest()); code != exitFail || !strings.Contains(errs, "is pinned") {
		t.Errorf("delete --local of the pin: %d %q", code, errs)
	}
	if code, _, errs := cmd("", "delete", one.Tag); code != exitUsage || !strings.Contains(errs, "one of --local and --registry") {
		t.Errorf("delete without --local or --registry: %d %q", code, errs)
	}
	if code, out, errs := cmd("", "delete", "--local", two.Tag); code != exitOK || image.Present(two.Pin.Ref) {
		t.Errorf("delete --local: %d %q\n%s", code, errs, out)
	}
	if code, out, _ := cmd("no\n", "delete", "--registry", two.Pin.Digest()); code != exitFail || !strings.Contains(out, "can't be undone") ||
		f.Reg.ManifestDigest("michael/plinth-"+ns.Repo, two.Tag) == "" {
		t.Errorf("delete --registry, not confirmed: %d\n%s", code, out)
	}
	if code, _, errs := cmd("yes\n", "delete", "--registry", two.Pin.Digest()); code != exitOK || f.Reg.ManifestDigest("michael/plinth-"+ns.Repo, two.Tag) != "" {
		t.Errorf("delete --registry: %d %q", code, errs)
	}

	// pull: the pin, after this machine's copy went.
	image.RemoveLocal(one.Pin.Ref)
	if code, out, errs := run(append([]string{"image", "pull"}, nsArgs...)...); code != exitOK || !image.Present(one.Pin.Ref) {
		t.Errorf("pull: %d %q\n%s", code, errs, out)
	}
	logs, _ := os.ReadDir(filepath.Join(work, "logs"))
	var log strings.Builder
	for _, e := range logs {
		b, _ := os.ReadFile(filepath.Join(work, "logs", e.Name()))
		log.Write(b)
	}
	for _, want := range []string{`msg="image pinned"`, "ref=" + one.Pin.Ref, `msg="image: removed this machine's copy"`, `msg="image deleted from the registry"`,
		"ref=" + two.Pin.Ref, `msg="image pulled"`, "digest=" + one.Pin.Ref[strings.Index(one.Pin.Ref, "@")+1:]} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("no %q in the general log:\n%s", want, log.String())
		}
	}
}
