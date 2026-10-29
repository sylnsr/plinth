# plinthctl build (ai/plinthctl.README.md, Source and build). build.sh runs it with --network none and exports the
# final stage, which holds only the binary.

# Stage 1: copy src/, check, test and compile. A failing step fails the build.
# Pinned by tag and digest (the multi-arch index of golang:1.26 on 2026-10-12). To update: change both together.
FROM docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89 AS build

# build.sh passes git describe --tags --always; the build context has no .git.
ARG VERSION=dev

# Offline and reproducible: never download a toolchain or a module, static binary.
ENV GOTOOLCHAIN=local \
    GOPROXY=off \
    CGO_ENABLED=0

WORKDIR /src
COPY src/ ./

# Only the allowlisted Go sources may arrive (.containerignore). Anything else, a config file above all, fails the
# build, so a broken .containerignore can't go unnoticed. The one exception: the config templates the binary embeds.
RUN set -eu; \
    extra="$(ls -A | grep -v -x -e go.mod -e go.sum -e cmd -e internal -e vendor || true)"; \
    configs="$(find . -path ./vendor -prune -o \( -name '*.yml' -o -name '*.yaml' -o -name '*.yml.*' -o -name '.claude' \) ! -path './internal/workdir/config.yml.dist' ! -path './internal/workdir/ns-config.yml.dist' -print)"; \
    if [ -n "$extra$configs" ]; then echo "not allowed in the build context:"; printf '%s\n' $extra $configs; exit 1; fi

# RUN steps are POSIX sh with set -eu, no SHELL instruction: Podman's default OCI format ignores SHELL, and bash syntax
# under /bin/sh made the gofmt check pass without checking anything.
# gofmt -l exits 0 even when it lists files, so check its output. Only our code: vendor/ is upstream's, as it was released.
RUN set -eu; \
    unformatted="$(gofmt -l cmd internal)"; \
    if [ -n "$unformatted" ]; then echo "not gofmt-ed:"; echo "$unformatted"; exit 1; fi
# go mod verify needs the module cache, which this offline build doesn't have once vendor/ exists: vendor/ is checked
# against go.sum and the checksum database when it is made (ai/plinthctl.README.md, Source and build, Vendoring), and
# -mod=vendor makes every go command below check vendor/modules.txt against go.mod.
RUN set -eu; if [ ! -d vendor ]; then go mod verify; fi
RUN go vet ./...
RUN go test ./...
RUN set -eu; \
    mod=-mod=readonly; \
    if [ -d vendor ]; then mod=-mod=vendor; fi; \
    go build -trimpath "$mod" \
      -ldflags "-X plinth/internal/cli.Version=${VERSION}" \
      -o /out/plinthctl ./cmd/plinthctl
RUN /out/plinthctl version

# Stage 2: the binary only.
FROM scratch
COPY --from=build /out/plinthctl /plinthctl
ENTRYPOINT ["/plinthctl"]
