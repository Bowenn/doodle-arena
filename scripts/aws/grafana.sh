#!/usr/bin/env bash
# Grafana on the data host, at http://localhost:3000, through a Session Manager port forward: no
# public port, and only someone signed in to the AWS account gets there. Needs the Session
# Manager plugin (docs/setup.md, "AWS"). Ctrl-C closes it.
source "$(dirname "$0")/common.sh"
require_aws
init_stack
aws ssm start-session --target "$(tf_output data_host_instance_id)" \
  --document-name AWS-StartPortForwardingSession --parameters portNumber=3000,localPortNumber="${GRAFANA_PORT:-3000}"
