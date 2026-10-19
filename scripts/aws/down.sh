#!/usr/bin/env bash
# Ends a session (ADR 0003): destroys the whole stack, data included. The bootstrap root (state,
# images, third-party secrets) and the budget stay.
#   DOMAIN=example.com scripts/aws/down.sh
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack
terraform -chdir="$TF/stack" destroy "${TF_STACK_VARS[@]}"
