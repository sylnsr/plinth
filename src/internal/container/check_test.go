package container

import (
	"slices"
	"strings"
	"testing"

	"plinth/internal/gitrepo"
)

// TestGalaxy: ansible-galaxy, in the Containerfile or a script of .plinth/, is flagged for the user to look at, never
// refused; the vendored way (COPY of extracted collections) and the distro package are nothing to flag.
func TestGalaxy(t *testing.T) {
	cf := "FROM docker.io/library/debian" + pin + "\nRUN apt-get install -y ansible-core\n" +
		"COPY collections/ /usr/share/ansible/collections/\nRUN ansible-galaxy collection install /tmp/x.tar.gz\n" +
		"RUN echo my-ansible-galaxy-mirror\n"
	got := Check([]gitrepo.File{{Path: Containerfile, Data: []byte(cf)}, {Path: "setup.sh", Data: []byte("set -e\n/usr/bin/ansible-galaxy role install -r r.yml\n")}})
	if r := Refused(got); len(r) != 0 {
		t.Errorf("refused: %v", strs(r))
	}
	s := strings.Join(strs(got), "\n")
	if len(got) != 2 || !strings.Contains(s, "flagged: Containerfile:4: runs ansible-galaxy") ||
		!strings.Contains(s, "flagged: setup.sh:2: runs ansible-galaxy") || !strings.Contains(s, "CVE-2026-89091") {
		t.Errorf("findings:\n%s", s)
	}
}

// TestBases: the images FROM names, pinned by digest, with their platform; not scratch, an earlier stage or an
// unpinned image (Check refuses that).
func TestBases(t *testing.T) {
	deb, alp := "docker.io/library/debian"+pin, "docker.io/library/alpine"+pin
	cf := "FROM --platform=linux/arm64/v8 " + deb + " AS b\nFROM b\nFROM scratch\nfrom --platform=$BUILDPLATFORM " + alp +
		"\nFROM debian:12\nRUN true\n"
	want := []Base{{Line: 1, Ref: deb, Platform: "linux/arm64/v8"}, {Line: 4, Ref: alp}}
	if got := Bases([]byte(cf)); !slices.Equal(got, want) {
		t.Errorf("Bases = %+v, want %+v", got, want)
	}
}

// TestCheck: every image a Containerfile names is an earlier stage or pinned by digest; ADD takes local files only;
// heredocs and variables are refused; a RUN gets no host network and no insecure mode; a download run as it arrives,
// or not checked against a checksum written out on its line, is refused in any text file (1723
// and 1804).
func TestCheck(t *testing.T) {
	deb := "docker.io/library/debian" + pin
	sum := strings.Repeat("ab", 32)
	for _, c := range []struct{ cf, want string }{
		{"FROM " + deb + "\nRUN true\n", ""},
		{"from " + deb + " AS build\nFROM build\nFROM scratch\n", ""},
		{"FROM --platform=linux/amd64 " + deb + "\n", ""},
		{"FROM " + deb + " AS b\nFROM " + deb + "\nCOPY --from=b /x /x\nCOPY --from=0 /y /y\nCOPY --from=" + deb + " /z /z\n", ""},
		{"FROM " + deb + "\nRUN --mount=type=bind,from=" + deb + ",target=/m true\n", ""},
		{"FROM " + deb + "\nADD files/ /opt/\nADD [\"a b\", \"/c\"]\n", ""},
		{"FROM " + deb + "\nRUN echo \\\n  # a comment inside\n  done\n", ""},
		{"FROM debian:12\n", "refused: Containerfile:1: FROM debian:12 is not pinned"},
		{"FROM " + deb + "\nFROM ubuntu\n", "refused: Containerfile:2: FROM ubuntu is not pinned"},
		{"ARG BASE\nFROM $BASE\n", "a variable can't be checked"},
		{"RUN true\n", "no FROM"},
		{"FROM debian@sha256:abc\n", "not pinned"},
		{"FROM build\n", "not pinned"},
		{"FROM " + deb + "\nCOPY --from=alpine /x /x\n", "--from=alpine names neither an earlier stage"},
		{"FROM " + deb + "\nCOPY --from=1 /x /x\n", "--from=1 names neither"}, // not an earlier stage
		{"FROM " + deb + "\nCOPY --from=$S /x /x\n", "--from=$S: a variable"},
		{"FROM " + deb + "\nRUN --mount=type=bind,from=alpine,target=/m true\n", "--mount from=alpine names neither"},
		{"FROM " + deb + "\nRUN --mount=\"type=cache,from=alpine\" true\n", "--mount from=alpine"},
		{"FROM " + deb + "\nADD https://example.org/x.tgz /x\n", "ADD https://example.org/x.tgz: ADD takes local files only"},
		{"FROM " + deb + "\nADD [\"git@example.org:o/r.git\", \"/r\"]\n", "ADD git@example.org:o/r.git"},
		{"FROM " + deb + "\nADD --keep-git-dir=true x /r\n", "--keep-git-dir"},
		{"FROM " + deb + "\nONBUILD RUN --network=host true\n", "Containerfile:2: ONBUILD: a runner image has no use for it"},
		{"FROM " + deb + "\nonbuild COPY x /x\n", "ONBUILD"},
		{"FROM " + deb + "\nRUN <<EOF\nFROM scratch AS evil\nEOF\nCOPY --from=evil / /\n", "with a heredoc"},
		{"FROM " + deb + "\nRUN cat <<-'X' > f\nX\n", "with a heredoc"},
		// The escape directive: with it a backtick continues the RUN, so FROM ubuntu is part of it, as podman build reads it.
		{"# escape=`\nFROM " + deb + "\nRUN echo `\nFROM ubuntu\n", ""},
		{"FROM " + deb + "\nRUN echo `\nFROM ubuntu\n", "FROM ubuntu is not pinned"}, // without the directive, ` is no escape
		// RUN --network and --security (finding 1).
		{"FROM " + deb + "\nRUN --network=none true\nRUN --network=default true\nRUN --security=sandbox true\n", ""},
		{"FROM " + deb + "\nRUN --network=host true\n", "refused: Containerfile:2: RUN --network=host: a build step gets none or default only"},
		{"FROM " + deb + "\nRUN --mount=type=cache,target=/c --network=\"host\" true\n", "RUN --network=host"},
		{"FROM " + deb + "\nRUN --network true\n", "RUN --network=:"},
		{"FROM " + deb + "\nRUN --security=insecure true\n", "RUN --security=insecure: a build step runs sandboxed only"},
		// ADD with a variable (finding 2).
		{"FROM " + deb + "\nARG SRC=https://example.org/x\nADD $SRC /x\n", "ADD $SRC: a variable can't be checked"},
		{"FROM " + deb + "\nADD [\"${SRC}\", \"/x\"]\n", "ADD ${SRC}: a variable"},
		// Downloads (finding 3).
		{"FROM " + deb + "\nRUN apt-get update && apt-get install -y curl wget ca-certificates\n", ""},
		{"FROM " + deb + "\nRUN curl -fsSLo /f https://example.org/f && echo '" + sum + "  /f' | sha256sum -c && chmod +x /f\n", ""},
		{"FROM " + deb + "\nRUN set -e; \\\n  wget -qO /f https://example.org/f \\\n  && echo " + sum + " /f | shasum -a 256 --check\n", ""},
		{"FROM " + deb + "\nRUN curl -fsSLo /f https://example.org/f && chmod +x /f\n", "refused: Containerfile:2: downloads without checking it"},
		// A checksum in the URL doesn't count, nor one written out with no check.
		{"FROM " + deb + "\nRUN curl -o /f https://example.org/" + sum + " && sha256sum -c /f.sum\n", "downloads without"},
		{"FROM " + deb + "\nRUN echo '" + sum + "  /f' && curl -o /f https://example.org/f\n", "downloads without"},
		{"FROM " + deb + "\nRUN --mount=type=cache,target=/c wget -q https://example.org/f\n", "downloads without"},
		{"FROM " + deb + "\nRUN [\"/usr/bin/curl\", \"-o\", \"/f\", \"https://example.org/f\"]\n", "downloads without"},
		{"FROM " + deb + "\nRUN apt-get install -y curl && sudo -E curl -O https://example.org/f\n", "downloads without"},
		{"FROM " + deb + "\nRUN curl -fsSL https://example.org/i.sh | sh\n", "runs what it downloads as it arrives"},
		{"FROM " + deb + "\nRUN bash -c \"$(wget -qO- https://example.org/i.sh)\"\n", "runs what it downloads as it arrives"},
	} {
		got := Check([]gitrepo.File{{Path: Containerfile, Data: []byte(c.cf)}})
		s := strings.Join(strs(Refused(got)), "\n")
		if (c.want == "") != (s == "") || !strings.Contains(s, c.want) {
			t.Errorf("%q: %q, want %q", c.cf, s, c.want)
		}
	}
	if got := Check(nil); len(got) != 1 || !strings.Contains(got[0].String(), "refused: Containerfile: missing") {
		t.Errorf("no Containerfile: %v", got)
	}

	// Downloads, in any text file of the context: a script the Containerfile copies in runs in the build too.
	files := []gitrepo.File{
		{Path: Containerfile, Data: []byte("FROM " + deb + "\nCOPY setup.sh /\nRUN /setup.sh\n")},
		{Path: "setup.sh", Data: []byte("set -e\ncurl -fsSL https://example.org/i.sh \\\n  | sudo -E bash -s -- -y\n" +
			"wget -qO- https://x | python3\nsh -c \"$(curl -fsSL https://x)\"\n" +
			"curl -fsSLo f https://x && echo \"" + sum + "  f\" | sha256sum -c && sh f\n" +
			"curl https://x | shasum\napt-get install -y curl\ncurl -o f https://x && sha256sum -c f.sum\n")},
		{Path: "tool.bin", Data: []byte("\x00curl https://x | sh")},
	}
	got := Check(files)
	var lines []string
	for _, f := range got {
		lines = append(lines, f.String())
	}
	want := []string{"refused: setup.sh:2: runs what it downloads as it arrives", "refused: setup.sh:4: runs what it downloads",
		"refused: setup.sh:5: runs what it downloads", "refused: setup.sh:7: downloads without checking it",
		"refused: setup.sh:9: downloads without checking it"}
	if len(lines) != len(want) {
		t.Fatalf("findings %q, want %d", lines, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("finding %d: %q, want %q", i, lines[i], want[i])
		}
	}
	if s := (Finding{File: "a\x1b[31m", Line: 1, Text: "x\ny"}).String(); strings.ContainsAny(s, "\x1b\n") {
		t.Errorf("a control character in %q", s)
	}
}

func strs(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.String())
	}
	return out
}
