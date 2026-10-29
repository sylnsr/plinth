# Sourced by an agent's launcher script on the host, from the repo's top level, with AGENT_NAME set.
# Registers the agent with Plinth and exports a fresh PLINTH_TOKEN; the old token stops working.
if ! command -v plinthctl >/dev/null 2>&1; then
  echo "Error: plinthctl not found on PATH - can't register ${AGENT_NAME} with Plinth" >&2
  exit 1
fi
PLINTH_TOKEN="$(plinthctl register --repo "$PWD" --agent "$AGENT_NAME")" || {
  echo "Error: plinthctl register failed for agent ${AGENT_NAME}" >&2
  exit 1
}
if [[ ! "$PLINTH_TOKEN" =~ ^plinth_[A-Za-z0-9_-]{43}$ ]]; then
  echo "Error: plinthctl register gave no token for agent ${AGENT_NAME}" >&2
  exit 1
fi
export PLINTH_TOKEN
