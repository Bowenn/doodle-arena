#!/usr/bin/env bash
# The data host's Grafana at http://localhost:3300, and its collector's OTLP port at
# localhost:14318 (for k6's metrics during a load test, task 5.3), through Session Manager port
# forwards: no public port, and only someone signed in to the AWS account gets there. The local
# ports differ from the local stack's (3000, 4318), so both can run. Needs the Session Manager
# plugin (docs/setup.md, "AWS"). Ctrl-C closes both.
source "$(dirname "$0")/common.sh"
require_aws
init_stack
target="$(tf_output data_host_instance_id)"
forward() {
  aws ssm start-session --target "$target" --document-name AWS-StartPortForwardingSession \
    --parameters "portNumber=$1,localPortNumber=$2"
}
forward 4318 "${OTLP_PORT:-14318}" >/dev/null &
otlp=$!
trap 'kill "$otlp" 2>/dev/null' EXIT
echo "Grafana: http://localhost:${GRAFANA_PORT:-3300}   OTLP for k6: localhost:${OTLP_PORT:-14318}"
forward 3000 "${GRAFANA_PORT:-3300}"
