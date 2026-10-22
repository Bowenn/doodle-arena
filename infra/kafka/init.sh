#!/bin/bash
# One-shot job (ADR 0013), as the kafka-init super user: creates the topics from the event catalog
# (design doc 6.2), one SCRAM-SHA-512 user per service and the ACLs below. Safe to run again:
# topics use --if-not-exists, setting a password again keeps it, and an existing ACL is a no-op.
# Anything not granted here is denied (allow.everyone.if.no.acl.found=false).
#
# A user whose password variable is empty isn't created and gets no grants: dev-tools exists
# only locally and in CI, never in AWS. Removing a grant from this list doesn't revoke it on a
# running broker: the ACLs are printed at the end to check, and kafka-acls.sh --remove does it.
#
# admin is never used here: it exists only on the broker's localhost ADMIN listener, for humans
# (infra/kafka/broker.sh). A broker formatted before that (task 5.5's follow-up) still has a
# SCRAM admin credential and neither kafka-init nor broker: this job then creates those two with
# it, once, deletes it, and says so.
set -euo pipefail

BOOTSTRAP="${KAFKA_BOOTSTRAP:-kafka:9092}"
PARTITIONS="${KAFKA_PARTITIONS:-6}"
BIN=/opt/kafka/bin
ADMIN=(--bootstrap-server "$BOOTSTRAP" --command-config /tmp/admin.properties)

: "${KAFKA_INIT_PASSWORD:?}" "${KAFKA_BROKER_PASSWORD:?}" "${KAFKA_ADMIN_PASSWORD:?}"

client_config() { # user password file
  cat > "$3" <<EOF
security.protocol=${KAFKA_SECURITY_PROTOCOL:-SASL_PLAINTEXT}
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="$1" password="$2";
EOF
  if [[ -n "${KAFKA_CA_FILE:-}" ]]; then
    printf 'ssl.truststore.type=PEM\nssl.truststore.location=%s\n' "$KAFKA_CA_FILE" >> "$3"
  fi
}
client_config kafka-init "$KAFKA_INIT_PASSWORD" /tmp/admin.properties

if ! timeout 30 "$BIN/kafka-broker-api-versions.sh" "${ADMIN[@]}" >/dev/null 2>&1; then
  echo "kafka-init can't log in: this broker predates the kafka-init and broker users;" \
    "creating them with its SCRAM admin credential, then deleting that credential (once)"
  client_config admin "$KAFKA_ADMIN_PASSWORD" /tmp/bootstrap.properties
  for entry in "kafka-init:$KAFKA_INIT_PASSWORD" "broker:$KAFKA_BROKER_PASSWORD"; do
    "$BIN/kafka-configs.sh" --bootstrap-server "$BOOTSTRAP" --command-config /tmp/bootstrap.properties \
      --alter --entity-type users --entity-name "${entry%%:*}" \
      --add-config "SCRAM-SHA-512=[password=${entry#*:}]" >/dev/null
  done
  rm -f /tmp/bootstrap.properties
fi
# admin logs in only on the broker's localhost ADMIN listener (PLAIN): a SCRAM credential, left by
# an older broker, would let it in over the network, so it goes.
if "$BIN/kafka-configs.sh" "${ADMIN[@]}" --describe --entity-type users --entity-name admin 2>/dev/null |
  grep -q SCRAM-SHA-512; then
  "$BIN/kafka-configs.sh" "${ADMIN[@]}" --alter --entity-type users --entity-name admin \
    --delete-config SCRAM-SHA-512 >/dev/null
  echo "deleted admin's SCRAM credential: admin now logs in only on the ADMIN listener"
fi

TOPICS=(
  game.rounds.v1
  game.matches.v1
  assets.drawings.v1
  market.listings.v1
  market.sales.v1
  market.wallet.v1
  notifier.dlq.v1
  auth.users.v1
  live.guesses.v1
  live.top-guesses.v1
)

# principal -> the variable holding its password. broker and kafka-init are super users
# (infra/kafka/broker.sh), so they have no grants below; admin is never set here.
USERS=(
  broker:KAFKA_BROKER_PASSWORD
  kafka-init:KAFKA_INIT_PASSWORD
  game-server:KAFKA_GAME_SERVER_PASSWORD
  lobby:KAFKA_LOBBY_PASSWORD
  assets:KAFKA_ASSETS_PASSWORD
  marketplace:KAFKA_MARKETPLACE_PASSWORD
  notifier:KAFKA_NOTIFIER_PASSWORD
  auth:KAFKA_AUTH_PASSWORD
  kafka-metrics:KAFKA_METRICS_PASSWORD
  analytics:KAFKA_ANALYTICS_PASSWORD
  dev-tools:KAFKA_DEV_TOOLS_PASSWORD
)

# principal operation resources: ADR 0013's table. A producer gets Write on its topics, a
# consumer Read on its topics and its group (Read and Write imply Describe). Idempotent
# producers need nothing more (KIP-679).
GRANTS=$(cat <<'EOF'
game-server   Write    --topic game.rounds.v1 --topic game.matches.v1
lobby         Read     --topic game.matches.v1 --group lobby-leaderboard
assets        Read     --topic game.rounds.v1 --group assets
assets        Write    --topic assets.drawings.v1
marketplace   Read     --topic game.matches.v1 --topic assets.drawings.v1 --group marketplace
marketplace   Write    --topic market.listings.v1 --topic market.sales.v1 --topic market.wallet.v1
notifier      Read     --topic market.sales.v1 --topic market.wallet.v1 --topic auth.users.v1 --group notifier --group notifier-auth
notifier      Write    --topic notifier.dlq.v1
auth          Write    --topic auth.users.v1
kafka-metrics Describe --topic * --group *
analytics     Read     --topic live.guesses.v1 --group analytics
analytics     Write    --topic live.top-guesses.v1
dev-tools     Read     --topic *
dev-tools     Read     --group dev-tools- --resource-pattern-type prefixed
dev-tools     Write    --topic game.rounds.v1 --topic game.matches.v1 --topic live.guesses.v1
EOF
)

password_of() {
  local entry
  for entry in "${USERS[@]}"; do
    if [[ "${entry%%:*}" == "$1" ]]; then
      local var="${entry#*:}"
      printf '%s' "${!var:-}"
      return
    fi
  done
}

# The resource lists contain a literal * (every topic, every group): no globbing.
set -f

pids=()
for topic in "${TOPICS[@]}"; do
  "$BIN/kafka-topics.sh" "${ADMIN[@]}" --create --if-not-exists \
    --topic "$topic" --partitions "$PARTITIONS" --replication-factor 1 >/dev/null &
  pids+=($!)
done
for pid in "${pids[@]}"; do wait "$pid"; done

# One JVM per call, so the users go in parallel.
pids=()
for entry in "${USERS[@]}"; do
  name="${entry%%:*}"
  password="$(password_of "$name")"
  if [[ -z "$password" ]]; then
    echo "user $name: no password set, skipped"
    continue
  fi
  "$BIN/kafka-configs.sh" "${ADMIN[@]}" --alter --entity-type users --entity-name "$name" \
    --add-config "SCRAM-SHA-512=[password=${password}]" >/dev/null &
  pids+=($!)
done
for pid in "${pids[@]}"; do wait "$pid"; done

pids=()
while read -r principal operation resources; do
  [[ -z "$principal" ]] && continue
  [[ -z "$(password_of "$principal")" ]] && continue
  # shellcheck disable=SC2086 # resources is a list of flags
  "$BIN/kafka-acls.sh" "${ADMIN[@]}" --add --allow-principal "User:$principal" \
    --operation "$operation" $resources >/dev/null &
  pids+=($!)
done <<< "$GRANTS"
for pid in "${pids[@]}"; do wait "$pid"; done

# Grants removed from the table since a broker's volume was formatted. kafka-metrics needs
# Describe on topics and groups only (its scrapers are consumers and topics); Describe on the
# cluster would also let it read the whole ACL table (Stage 5 review).
"$BIN/kafka-acls.sh" "${ADMIN[@]}" --remove --force --allow-principal User:kafka-metrics \
  --operation Describe --cluster >/dev/null

echo "topics:"
"$BIN/kafka-topics.sh" "${ADMIN[@]}" --list
echo "ACLs:"
"$BIN/kafka-acls.sh" "${ADMIN[@]}" --list | sed -n 's/.*resourceType=\([A-Z_]*\), name=\([^,]*\), patternType=\([A-Z]*\).*/\1 \2 (\3)/p; s/.*principal=User:\([^,]*\), host=[^,]*, operation=\([A-Z_]*\).*/    \1 \2/p'
