#!/bin/bash
# The negative half of ADR 0013's test, run by scripts/kafka-acl-check.sh inside a one-off
# kafka-init container (on the Kafka network, with every user's password): each case is
# something a service must not be able to do, and it must fail with the right error. The happy
# path is make pipeline-check and the stack's own traffic. Nothing here writes a record: every
# write it tries is refused before a record is accepted.
set -uo pipefail

BIN=/opt/kafka/bin
BOOTSTRAP=kafka:9092
failures=0

props() { # user password [name] -> a client properties file
  local file="/tmp/${3:-$1}.properties"
  cat > "$file" <<EOF
security.protocol=SASL_PLAINTEXT
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="$1" password="$2";
request.timeout.ms=10000
default.api.timeout.ms=15000
EOF
  echo "$file"
}

# expect "description" "pattern" command...: the command must fail and print the pattern.
expect() {
  local what="$1" pattern="$2" out code
  shift 2
  out="$(timeout 40 "$@" 2>&1 < /dev/null)"
  code=$?
  if [[ $code -ne 0 || "$out" == *ERROR* ]] && grep -qiE "$pattern" <<< "$out"; then
    echo "  ok    $what"
  else
    echo "  FAIL  $what (exit $code)"
    grep -iE "error|exception|denied" <<< "$out" | head -3 | sed 's/^/          /'
    failures=$((failures + 1))
  fi
}

# A refused connection never lists the broker's API versions ("Produce(0): 0 to 13 ..."); a
# hang until the timeout is a refusal too (the SASL listener drops a client that skips the
# handshake). That alone would also pass with the broker down or a wrong address, so the same
# call with a valid credential (the control, $1) must list them first.
expect_no_connection() {
  local control="$1" what="$2" out
  shift 2
  out="$(timeout 20 "$@" --command-config "$control" 2>&1 < /dev/null)"
  if ! grep -q "Produce(0)" <<< "$out"; then
    echo "  FAIL  $what: the broker didn't answer a valid credential either, so this proves nothing"
    failures=$((failures + 1))
    return
  fi
  out="$(timeout 20 "$@" 2>&1 < /dev/null)"
  if grep -q "Produce(0)" <<< "$out"; then
    echo "  FAIL  $what"
    failures=$((failures + 1))
  else
    echo "  ok    $what"
  fi
}

lobby="$(props lobby "$KAFKA_LOBBY_PASSWORD")"
assets="$(props assets "$KAFKA_ASSETS_PASSWORD")"
marketplace="$(props marketplace "$KAFKA_MARKETPLACE_PASSWORD")"
notifier="$(props notifier "$KAFKA_NOTIFIER_PASSWORD")"
game_server="$(props game-server "$KAFKA_GAME_SERVER_PASSWORD")"
wrong="$(props marketplace "not-the-password" wrong-password)"
network_admin="$(props admin "$KAFKA_ADMIN_PASSWORD" network-admin)"

echo "Kafka ACLs (ADR 0013):"
expect_no_connection "$lobby" "a client with no credential is refused" \
  "$BIN/kafka-broker-api-versions.sh" --bootstrap-server "$BOOTSTRAP"
expect "a wrong password is refused" "Authentication failed" \
  "$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$wrong" --list
expect "admin can't log in over the network (only on the broker's localhost listener)" "Authentication failed" \
  "$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$network_admin" --list
expect "lobby can't write game.matches.v1 (mint coins)" "TOPIC_AUTHORIZATION_FAILED|Topic authorization failed|Not authorized to access topics" \
  bash -c "echo x | $BIN/kafka-console-producer.sh --bootstrap-server $BOOTSTRAP --command-config $lobby --topic game.matches.v1 --producer-property enable.idempotence=false"
expect "notifier can't write game.matches.v1" "TOPIC_AUTHORIZATION_FAILED|Topic authorization failed|Not authorized to access topics" \
  bash -c "echo x | $BIN/kafka-console-producer.sh --bootstrap-server $BOOTSTRAP --command-config $notifier --topic game.matches.v1 --producer-property enable.idempotence=false"
expect "assets can't write game.matches.v1" "TOPIC_AUTHORIZATION_FAILED|Topic authorization failed|Not authorized to access topics" \
  bash -c "echo x | $BIN/kafka-console-producer.sh --bootstrap-server $BOOTSTRAP --command-config $assets --topic game.matches.v1 --producer-property enable.idempotence=false"
expect "game-server can't write assets.drawings.v1 (mint a drawing)" "TOPIC_AUTHORIZATION_FAILED|Topic authorization failed|Not authorized to access topics" \
  bash -c "echo x | $BIN/kafka-console-producer.sh --bootstrap-server $BOOTSTRAP --command-config $game_server --topic assets.drawings.v1 --producer-property enable.idempotence=false"
expect "assets can't join group marketplace (skip its records)" "Not authorized to access group|GroupAuthorization" \
  "$BIN/kafka-console-consumer.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$assets" \
  --topic game.rounds.v1 --group marketplace --max-messages 1 --timeout-ms 10000
expect "marketplace can't create a topic" "Authorization failed|TopicAuthorization|not authorized" \
  "$BIN/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$marketplace" \
  --create --topic acl-check.v1 --partitions 1 --replication-factor 1
expect "game-server can't change a topic's retention" "Authorization failed|TopicAuthorization|ClusterAuthorization|not authorized" \
  "$BIN/kafka-configs.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$game_server" \
  --alter --entity-type topics --entity-name game.matches.v1 --add-config retention.ms=1
expect "game-server can't read game.matches.v1" "TOPIC_AUTHORIZATION_FAILED|Not authorized to access topics|TopicAuthorization" \
  "$BIN/kafka-console-consumer.sh" --bootstrap-server "$BOOTSTRAP" --command-config "$game_server" \
  --topic game.matches.v1 --partition 0 --offset earliest --max-messages 1 --timeout-ms 10000

if [[ $failures -gt 0 ]]; then
  echo "kafka-acl-check: $failures check(s) failed"
  exit 1
fi
echo "kafka-acl-check: OK"
