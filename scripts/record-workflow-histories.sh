#!/usr/bin/env bash
# Records Bill workflow histories into fees/billflow/testdata, for the replay
# test that keeps workflow changes compatible with bills already running.
#
# Usage: scripts/with-temporal.sh scripts/record-workflow-histories.sh <name>
#
# Writes <name>-close.json (adds, then closed by request), <name>-period-end.json
# (closed by its timer) and <name>-rollover.json (a run that continued as new).
# Keep the files of earlier versions: every recorded history must still replay.
set -euo pipefail

name=${1:?usage: $0 <name>}
out=fees/billflow/testdata
api=http://localhost:4000

encore run --port 4000 >/dev/null 2>&1 &
encore_pid=$!
trap 'kill "$encore_pid"' EXIT

post() {
	curl -sf -X POST "$api$1" -H 'Content-Type: application/json' \
		-H "Idempotency-Key: $(uuidgen)" -d "${2:-}"
}

create_bill() {
	post /v1/bills "{\"customer_id\": \"recorder\", \"currency\": \"USD\", \"period_end\": \"$1\"}" | jq -r .id
}

add_fee() {
	post "/v1/bills/$1/line-items" "{\"description\": \"fee\", \"amount\": \"$2\", \"currency\": \"$3\"}" >/dev/null
}

# wait_for <description> <command...> retries the command for up to 30 seconds.
wait_for() {
	local what=$1
	shift
	for _ in $(seq 1 60); do
		"$@" && return
		sleep 0.5
	done
	echo "timed out waiting for $what" >&2
	exit 1
}

workflow_completed() {
	[ "$(temporal workflow describe --workflow-id "$1" --output json | jq -r .workflowExecutionInfo.status)" = WORKFLOW_EXECUTION_STATUS_COMPLETED ]
}

save() {
	temporal workflow show --workflow-id "$1" ${3:+--run-id "$3"} --output json |
		jq --arg host "$(hostname)" 'walk(if type == "string" then gsub($host; "recorder") else . end)' \
			>"$out/$name-$2.json"
}

wait_for "the API" curl -sf "$api/v1/bills?customer_id=probe" -o /dev/null
mkdir -p "$out"

bill=$(create_bill "$(date -u -d '+1 day' +%FT%TZ)")
add_fee "$bill" 1050 USD
if add_fee "$bill" 500 GEL 2>/dev/null; then
	echo "a GEL line item was accepted on a USD bill" >&2
	exit 1
fi
add_fee "$bill" 295 USD
post "/v1/bills/$bill/close" >/dev/null
wait_for "$bill to complete" workflow_completed "$bill"
save "$bill" close

bill=$(create_bill "$(date -u -d '+3 seconds' +%FT%TZ)")
add_fee "$bill" 700 USD
wait_for "$bill to complete" workflow_completed "$bill"
save "$bill" period-end

bill=$(create_bill "$(date -u -d '+1 day' +%FT%TZ)")
first_run=$(temporal workflow describe --workflow-id "$bill" --output json | jq -r .workflowExecutionInfo.execution.runId)
for _ in $(seq 1 30); do add_fee "$bill" 10 USD; done
post "/v1/bills/$bill/close" >/dev/null
save "$bill" rollover "$first_run"
jq -e '.events[-1].eventType | test("CONTINUED_AS_NEW|ContinuedAsNew")' "$out/$name-rollover.json" >/dev/null || {
	echo "the rollover bill did not continue as new; record against the server scripts/with-temporal.sh starts" >&2
	exit 1
}
