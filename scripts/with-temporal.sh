#!/usr/bin/env bash
# Runs a command with a Temporal dev server available on localhost:7233.
# Reuses a server that is already running; otherwise starts an in-memory one
# and stops it when the command exits.
#
# The server it starts suggests continue-as-new after 40 history events
# (the default is 4096), so that the tests exercise bills whose workflow
# rolls over to a new run.
set -euo pipefail

temporal_is_up() {
	temporal operator cluster health --address localhost:7233 >/dev/null 2>&1
}

if ! temporal_is_up; then
	temporal server start-dev --headless --log-level error \
		--dynamic-config-value limit.historyCount.suggestContinueAsNew=40 &
	server_pid=$!
	trap 'kill "$server_pid"' EXIT

	for _ in $(seq 1 60); do
		temporal_is_up && break
		kill -0 "$server_pid" 2>/dev/null || { echo "temporal dev server exited" >&2; exit 1; }
		sleep 0.5
	done
	temporal_is_up || { echo "temporal dev server did not become healthy" >&2; exit 1; }
fi

"$@"
