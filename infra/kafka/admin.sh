#!/bin/bash
# kafka-admin: runs a Kafka CLI tool as admin, the super user for humans (ADR 0013). admin logs in
# only here, inside the broker container, on the localhost ADMIN listener, and every login raises
# the KafkaAdminUsed alert (docs/observability.md): use it on purpose, and say so. For example:
#   docker compose -f infra/docker-compose.yml --env-file .env exec kafka kafka-admin kafka-acls.sh --list
set -euo pipefail
: "${KAFKA_ADMIN_PASSWORD:?}"
tool="${1:?usage: kafka-admin <tool>.sh [args...]}"
shift
config="$(mktemp)"
trap 'rm -f "$config"' EXIT
cat > "$config" <<EOF
security.protocol=SASL_PLAINTEXT
sasl.mechanism=PLAIN
sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule required username="admin" password="${KAFKA_ADMIN_PASSWORD}";
EOF
"/opt/kafka/bin/$tool" --bootstrap-server localhost:9095 --command-config "$config" "$@"
