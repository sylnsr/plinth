#!/usr/bin/env bash
# Builds plinthctl in rootless Podman (ai/plinthctl.README.md, Source and build), using the two-stage Containerfile:
#   stage 1 copies src/ and runs gofmt, go mod verify, go vet, go test, go build; stage 2 holds only the binary.
# RUN steps have no network. A failing step fails the build.
# The version is git describe --tags --always --dirty of this repo, passed as a build argument (the context has no .git).
# CHANGES: builds the image plinth:dev, writes out/plinthctl. Nothing else.
set -euo pipefail
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
cd "$SCRIPT_DIR" || {
  echo "Error: Failed to change directory to $SCRIPT_DIR" >&2
  exit 1
}
# end bash header

IMAGE="plinth:dev"

VERSION="$(git describe --tags --always --dirty)"
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._+-]+$ ]]; then
  echo "Error: unexpected version '$VERSION' from git describe" >&2
  exit 1
fi

# A failed build leaves out/plinthctl as it was, and says so: plinthctl (the AAA library, plinthctl finish) keeps
# working meanwhile. The new binary is built beside it and renamed into place, so nothing sees it half written.
mkdir -p out
TMP_OUT="$(mktemp -d "$SCRIPT_DIR/out/.build.XXXXXX")"
trap 'rm -rf "$TMP_OUT"' EXIT

# .containerignore limits the context to src/. --output writes the final stage's files (only /plinthctl) into $TMP_OUT.
if ! podman build --network none --build-arg VERSION="$VERSION" -t "$IMAGE" --output type=local,dest="$TMP_OUT" -f Containerfile . ||
  [[ ! -x "$TMP_OUT/plinthctl" ]]; then
  echo "Error: the build failed; out/plinthctl is unchanged" >&2
  exit 1
fi
mv -f "$TMP_OUT/plinthctl" out/plinthctl
echo "Built: $SCRIPT_DIR/out/plinthctl ($(out/plinthctl version))"
