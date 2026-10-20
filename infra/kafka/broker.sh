#!/bin/bash
# The Kafka broker with SCRAM-SHA-512 users and ACLs (ADR 0013), in place of the image's own
# start script: it writes server.properties, formats the storage once with the admin's SCRAM
# credential (KIP-900: the first credential has to exist before anyone can authenticate), then
# starts the broker. kafka-init creates the services' users and ACLs as admin.
#
# Listeners:
#   INTERNAL    9092  SASL over the Kafka network (SASL_SSL in AWS, KAFKA_INTERNAL_PROTOCOL)
#   EXTERNAL    9094  SASL_PLAINTEXT on 127.0.0.1 for host tools (local only, KAFKA_EXTERNAL=1)
#   CONTROLLER  9093  the KRaft quorum, on localhost only: SASL/PLAIN with a static admin user,
#                     because SCRAM credentials live in the metadata log the controller serves
# Anything without an ACL is denied; admin is the only super user.
set -euo pipefail

: "${KAFKA_ADMIN_PASSWORD:?}"
: "${KAFKA_ADVERTISED_HOST:=kafka}"
: "${KAFKA_INTERNAL_PROTOCOL:=SASL_PLAINTEXT}"
: "${KAFKA_EXTERNAL:=0}"
: "${KAFKA_CLUSTER_ID:=5L6g3nShT-eMCtK--X86sw}"
DATA=/var/lib/kafka/data/kraft
CONFIG=/tmp/server.properties

listeners="INTERNAL://:9092,CONTROLLER://localhost:9093"
advertised="INTERNAL://${KAFKA_ADVERTISED_HOST}:9092"
protocols="INTERNAL:${KAFKA_INTERNAL_PROTOCOL},CONTROLLER:SASL_PLAINTEXT"
if [[ "$KAFKA_EXTERNAL" == 1 ]]; then
  listeners+=",EXTERNAL://:9094"
  advertised+=",EXTERNAL://localhost:9094"
  protocols+=",EXTERNAL:SASL_PLAINTEXT"
fi
scram_admin="org.apache.kafka.common.security.scram.ScramLoginModule required username=\"admin\" password=\"${KAFKA_ADMIN_PASSWORD}\";"
plain_admin="org.apache.kafka.common.security.plain.PlainLoginModule required username=\"admin\" password=\"${KAFKA_ADMIN_PASSWORD}\" user_admin=\"${KAFKA_ADMIN_PASSWORD}\";"

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
listener.name.internal.scram-sha-512.sasl.jaas.config=${scram_admin}
listener.name.external.scram-sha-512.sasl.jaas.config=${scram_admin}
sasl.mechanism.controller.protocol=PLAIN
listener.name.controller.sasl.enabled.mechanisms=PLAIN
listener.name.controller.plain.sasl.jaas.config=${plain_admin}

authorizer.class.name=org.apache.kafka.metadata.authorizer.StandardAuthorizer
allow.everyone.if.no.acl.found=false
super.users=User:admin

offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
group.initial.rebalance.delay.ms=0
# Topics are created explicitly (task 1.2), never by a typo in a producer.
auto.create.topics.enable=false
EOF

if [[ "$KAFKA_INTERNAL_PROTOCOL" == SASL_SSL ]]; then
  : "${KAFKA_TLS_DIR:?}"
  cat >> "$CONFIG" <<EOF
ssl.keystore.type=PEM
ssl.keystore.location=${KAFKA_TLS_DIR}/server.pem
EOF
fi

# The admin client config for the healthcheck and kafka-init (the same credential).
cat > /tmp/admin.properties <<EOF
security.protocol=${KAFKA_INTERNAL_PROTOCOL}
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=${scram_admin}
EOF

# Format once. The data now lives under kraft/: a volume formatted before ADR 0013 has no admin
# credential, so it's left alone instead (delete the old files under /var/lib/kafka/data by hand).
if [[ ! -f "$DATA/meta.properties" ]]; then
  /opt/kafka/bin/kafka-storage.sh format --config "$CONFIG" --cluster-id "$KAFKA_CLUSTER_ID" \
    --add-scram "SCRAM-SHA-512=[name=admin,password=${KAFKA_ADMIN_PASSWORD}]"
fi

export KAFKA_LOG4J_OPTS="-Dlog4j2.configurationFile=/etc/doodle-arena/kafka-log4j2.yaml"
exec /opt/kafka/bin/kafka-server-start.sh "$CONFIG"
