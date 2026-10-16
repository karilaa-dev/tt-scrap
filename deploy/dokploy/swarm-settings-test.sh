#!/bin/sh
# Tests swarm-settings.sh check against matching, drifted, and invalid input.
set -eu
script="$(dirname "$0")/swarm-settings.sh"
settings="$(dirname "$0")/swarm-settings.json"
failed=0

# A service spec as docker service inspect prints it: zero values omitted.
spec=$(jq -c '[{ID: "x", Spec: {
	UpdateConfig: (.updateConfigSwarm | del(.Delay, .MaxFailureRatio)),
	RollbackConfig: (.rollbackConfigSwarm | del(.Delay, .MaxFailureRatio)),
	TaskTemplate: {ContainerSpec: {
		Image: "app", Healthcheck: .healthCheckSwarm, StopGracePeriod: .stopGracePeriodSwarm}}}}]' "$settings")

# expect <name> <exit status> <input>
expect() {
	out=$(printf '%s' "$3" | "$script" check 2>&1) && status=0 || status=$?
	if [ "$status" -ne "$2" ]; then
		echo "FAIL $1: exit $status, want $2: $out"; failed=1
	elif [ "$2" -ne 0 ] && printf '%s' "$out" | grep -q 'service matches'; then
		echo "FAIL $1: claims a match: $out"; failed=1
	else
		echo "ok   $1"
	fi
}

expect "matching spec" 0 "$spec"
expect "drift" 1 "$(printf '%s' "$spec" | jq -c '.[0].Spec.UpdateConfig.Order = "start-first"')"
expect "missing healthcheck" 1 "$(printf '%s' "$spec" | jq -c 'del(.[0].Spec.TaskTemplate.ContainerSpec.Healthcheck)')"
expect "invalid JSON" 2 "not-json
"
expect "empty input" 2 ""
expect "empty array" 2 "[]"
expect "object instead of array" 2 "$(printf '%s' "$spec" | jq -c '.[0]')"
expect "missing Spec" 2 '[{"ID":"x"}]'
expect "missing ContainerSpec" 2 '[{"Spec":{"TaskTemplate":{}}}]'
expect "two services" 2 "$(printf '%s' "$spec" | jq -c '. + .')"
expect "two documents" 2 "$spec$spec"

exit "$failed"
