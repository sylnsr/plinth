# Project Status

Read the specs before the code: `*.README.md`.

## Rules

- You run in a rootless podman container.
- Go standard library only for coding the Plinth app in `/src`. The one allowed dependency is `go.yaml.in/yaml/v3`, vendored, and only one level deep (nothing it pulls in). Any other package needs the maintainer's approval. This means no tview/tcell: the TUI is stdlib.
- No Node.js. Pip-free Python or Perl is fine for small utility scripts only, and only in the `ai/work/` dir.
- Containers for Plinth are rootless Podman.
- Fail closed: no Ollama means reviews and the DLP scan fail and need the two-step override. No YOLO modes.
- Don't use `jq`, and avoid tools with major CVEs. Don't create `.mcp.json`.

## Build and test

The user runs `./build.sh` (repo root) on the host and pastes the output; the binary lands in `out/plinthctl`. It builds the two-stage `Containerfile` with rootless Podman and `--network none`: stage 1 (`golang:1.26`) copies `src/` and runs gofmt (must list nothing), `go mod verify`, `go vet`, `go test` and `go build`; stage 2 (`scratch`) holds only the binary. Details: `plinthctl.README.md`, Source and build.

Don't install a Go toolchain where you run. Ask user to run `./build.sh`.
