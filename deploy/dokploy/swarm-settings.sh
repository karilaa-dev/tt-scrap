#!/bin/sh
# Builds and checks the Dokploy Swarm settings in swarm-settings.json.
#   swarm-settings.sh payload <applicationId>   body for POST /api/application.update
#   swarm-settings.sh sql <appName>             same change as a Dokploy database update
#   docker service inspect <service> | swarm-settings.sh check
set -eu
settings="${SETTINGS:-$(dirname "$0")/swarm-settings.json}"

case "${1:-}" in
payload)
	[ $# -eq 2 ] || { echo "usage: $0 payload <applicationId>" >&2; exit 2; }
	jq --arg id "$2" '{applicationId: $id} + .' "$settings"
	;;
sql)
	[ $# -eq 2 ] || { echo "usage: $0 sql <appName>" >&2; exit 2; }
	case "$2" in *[!A-Za-z0-9_-]*) echo "invalid appName" >&2; exit 2 ;; esac
	# Dollar quoting avoids escaping the JSON; reject settings containing the tag.
	jq -e 'tojson | contains("$json$") | not' "$settings" >/dev/null || { echo "settings contain \$json\$" >&2; exit 2; }
	jq -r --arg app "$2" '
		def lit: "$json$" + tojson + "$json$::json";
		"UPDATE application SET",
		"  \"healthCheckSwarm\" = \(.healthCheckSwarm | lit),",
		"  \"updateConfigSwarm\" = \(.updateConfigSwarm | lit),",
		"  \"rollbackConfigSwarm\" = \(.rollbackConfigSwarm | lit),",
		"  \"stopGracePeriodSwarm\" = \(.stopGracePeriodSwarm)",
		"WHERE \"appName\" = $app$\($app)$app$;"' "$settings"
	;;
check)
	# Docker omits zero values from service specs, so zeros are dropped before
	# comparing. Prints each differing field and exits 1 on any difference.
	# Input that is not inspect output for exactly one service exits 2.
	drift=$(jq -rs --slurpfile want "$settings" '
		def norm: if type == "object" then with_entries(select(.value != 0)) else . end;
		if length == 1 and (.[0] | type) == "array" and (.[0] | length) == 1
			and (.[0][0].Spec.TaskTemplate.ContainerSpec | type) == "object"
		then .[0][0].Spec else error("want docker service inspect output for one service") end
		| . as $s | $want[0] as $w |
		["Healthcheck", $s.TaskTemplate.ContainerSpec.Healthcheck, $w.healthCheckSwarm],
		["UpdateConfig", $s.UpdateConfig, $w.updateConfigSwarm],
		["RollbackConfig", $s.RollbackConfig, $w.rollbackConfigSwarm],
		["StopGracePeriod", $s.TaskTemplate.ContainerSpec.StopGracePeriod, $w.stopGracePeriodSwarm]
		| select((.[1] | norm) != (.[2] | norm))
		| "\(.[0]): have \(.[1] | tojson), want \(.[2] | tojson)"') || exit 2
	[ -n "$drift" ] || { echo "service matches swarm-settings.json"; exit 0; }
	echo "$drift"
	exit 1
	;;
*)
	sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
