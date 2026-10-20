#!/bin/bash
# The Kafka broker with SCRAM-SHA-512 users and ACLs (ADR 0013), in place of the image's own
# start script: it writes server.properties, formats the storage once with the first SCRAM
# credentials (KIP-900: they have to exist before anyone can authenticate), then starts the
# broker. Three super users, each with one job:
#   broker      the broker itself: its controller heartbeats and inter-broker requests
#   kafka-init  the init job, which creates the services' users and ACLs (infra/kafka/init.sh)
#   admin       humans only, and only inside this container, on the ADMIN listener: it has no
#               SCRAM credential, so it can't log in over the network at all. Every admin login
#               counts on that listener's successful-authentication metric and raises
#               KafkaAdminUsed (docs/observability.md). Use it with kafka-admin (infra/kafka/admin.sh).
#
# Listeners:
#   INTERNAL    9092  SASL over the Kafka network (SASL_SSL in AWS, KAFKA_INTERNAL_PROTOCOL)
#   EXTERNAL    9094  SASL_PLAINTEXT on 127.0.0.1 for host tools (local only, KAFKA_EXTERNAL=1)
#   CONTROLLER  9093  the KRaft quorum, on localhost only: SASL/PLAIN with a static broker user,
#                     because SCRAM credentials live in the metadata log the controller serves
#   ADMIN       9095  localhost only: SASL/PLAIN with a static admin user
# Anything without an ACL is denied.
set -euo pipefail

: "${KAFKA_ADMIN_PASSWORD:?}" "${KAFKA_INIT_PASSWORD:?}" "${KAFKA_BROKER_PASSWORD:?}"
: "${KAFKA_ADVERTISED_HOST:=kafka}"
: "${KAFKA_INTERNAL_PROTOCOL:=SASL_PLAINTEXT}"
: "${KAFKA_EXTERNAL:=0}"
: "${KAFKA_CLUSTER_ID:=5L6g3nShT-eMCtK--X86sw}"
DATA=/var/lib/kafka/data/kraft
CONFIG=/tmp/server.properties

listeners="INTERNAL://:9092,CONTROLLER://localhost:9093,ADMIN://localhost:9095"
advertised="INTERNAL://${KAFKA_ADVERTISED_HOST}:9092,ADMIN://localhost:9095"
protocols="INTERNAL:${KAFKA_INTERNAL_PROTOCOL},CONTROLLER:SASL_PLAINTEXT,ADMIN:SASL_PLAINTEXT"
if [[ "$KAFKA_EXTERNAL" == 1 ]]; then
  listeners+=",EXTERNAL://:9094"
  advertised+=",EXTERNAL://localhost:9094"
  protocols+=",EXTERNAL:SASL_PLAINTEXT"
fi
scram_broker="org.apache.kafka.common.security.scram.ScramLoginModule required username=\"broker\" password=\"${KAFKA_BROKER_PASSWORD}\";"
plain_broker="org.apache.kafka.common.security.plain.PlainLoginModule required username=\"broker\" password=\"${KAFKA_BROKER_PASSWORD}\" user_broker=\"${KAFKA_BROKER_PASSWORD}\";"
plain_admin="org.apache.kafka.common.security.plain.PlainLoginModule required user_admin=\"${KAFKA_ADMIN_PASSWORD}\";"

umask 077
cat > "$CONFIG" <<EOF
node.id=1
process.roles=broker,controller
controller.quorum.voters=1@localhost:9093
listeners=${listeners}
advertised.listeners=${advertised}
listener.security.protocol.map=${protocols}
inter.broker.listener.name=INTERNAL
controller.listener.names=CONTROLLER
log.dirs=${DATA}

sasl.enabled.mechanisms=SCRAM-SHA-512
sasl.mechanism.inter.broker.protocol=SCRAM-SHA-512
listener.name.internal.scram-sha-512.sasl.jaas.config=${scram_broker}
listener.name.external.scram-sha-512.sasl.jaas.config=${scram_broker}
sasl.mechanism.controller.protocol=PLAIN
listener.name.controller.sasl.enabled.mechanisms=PLAIN
listener.name.controller.plain.sasl.jaas.config=${plain_broker}
listener.name.admin.sasl.enabled.mechanisms=PLAIN
listener.name.admin.plain.sasl.jaas.config=${plain_admin}

authorizer.class.name=org.apache.kafka.metadata.authorizer.StandardAuthorizer
allow.everyone.if.no.acl.found=false
super.users=User:broker;User:kafka-init;User:admin

offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
group.initial.rebalance.delay.ms=0
# Topics are created explicitly (task 1.2), never by a typo in a producer.
auto.create.topics.enable=false
EOF

# The healthcheck (kafka-init writes its own client config): ApiVersions as the broker, or, on a
# volume from before the broker user existed, with the SCRAM admin credential that volume still
# has, until kafka-init adds the broker user and deletes that credential. Both run on INTERNAL,
# so neither counts as an admin login on the ADMIN listener.
cat > /tmp/admin.properties <<EOF
security.protocol=${KAFKA_INTERNAL_PROTOCOL}
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=${scram_broker}
EOF
sed 's/username="broker" password="[^"]*"/username="admin" password="'"${KAFKA_ADMIN_PASSWORD}"'"/' /tmp/admin.properties > /tmp/admin-fallback.properties
cat > /tmp/healthcheck.sh <<'EOF'
#!/bin/bash
for config in /tmp/admin.properties /tmp/admin-fallback.properties; do
  /opt/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server localhost:9092 --command-config "$config" >/dev/null 2>&1 && exit 0
done
exit 1
EOF

# AWS: the broker's key and certificate (one PEM file, the key in PKCS#8) from the stack's own
# CA (infra/terraform/stack/kafka.tf), which clients trust.
if [[ "$KAFKA_INTERNAL_PROTOCOL" == SASL_SSL ]]; then
  : "${KAFKA_TLS_DIR:?}"
  # The truststore is for the broker's own inter-broker client, which checks it at startup.
  cat >> "$CONFIG" <<EOF
ssl.keystore.type=PEM
ssl.keystore.location=${KAFKA_TLS_DIR}/server.pem
ssl.truststore.type=PEM
ssl.truststore.location=${KAFKA_TLS_DIR}/ca.pem
EOF
  for f in /tmp/admin.properties /tmp/admin-fallback.properties; do
    printf 'ssl.truststore.type=PEM\nssl.truststore.location=%s/ca.pem\n' "$KAFKA_TLS_DIR" >> "$f"
  done
fi

# Format once. The data now lives under kraft/: a volume formatted before ADR 0013 has no
# credential at all, so it's left alone instead (delete the old files under /var/lib/kafka/data
# by hand). A volume formatted before task 5.5's follow-up has a SCRAM admin and neither broker
# nor kafka-init: kafka-init migrates it once (infra/kafka/init.sh).
if [[ ! -f "$DATA/meta.properties" ]]; then
  /opt/kafka/bin/kafka-storage.sh format --config "$CONFIG" --cluster-id "$KAFKA_CLUSTER_ID" \
    --add-scram "SCRAM-SHA-512=[name=kafka-init,password=${KAFKA_INIT_PASSWORD}]" \
    --add-scram "SCRAM-SHA-512=[name=broker,password=${KAFKA_BROKER_PASSWORD}]"
fi

export KAFKA_LOG4J_OPTS="-Dlog4j2.configurationFile=/etc/doodle-arena/kafka-log4j2.yaml"
# The OpenTelemetry agent (infra/kafka/Dockerfile) for the broker's JVM only: KAFKA_OPTS is set
# here, not in the image, because every Kafka CLI tool (the healthcheck among them) reads it.
if [[ -f /opt/otel/opentelemetry-javaagent.jar && -n "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ]]; then
  export KAFKA_OPTS="-javaagent:/opt/otel/opentelemetry-javaagent.jar ${KAFKA_OPTS:-}"
fi
exec /opt/kafka/bin/kafka-server-start.sh "$CONFIG"
