#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
cd "$SCRIPT_DIR" || {
  echo "Error: Failed to change directory to $SCRIPT_DIR" >&2
  exit 1
}
# end bash header

cd ./src
CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off go build -o ../out/plinthctl ./cmd/plinthctl