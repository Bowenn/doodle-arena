#!/bin/bash
# One-shot job: creates the week-1 Kafka topics from the event catalog (design doc, section 6.2).
# Safe to run again. Auto-creation is off in the broker, so a topic missing here doesn't exist.
set -euo pipefail

BOOTSTRAP="${KAFKA_BOOTSTRAP:-kafka:9092}"
PARTITIONS="${KAFKA_PARTITIONS:-6}"

TOPICS=(
  game.rounds.v1
  game.matches.v1
  assets.drawings.v1
  market.listings.v1
  market.sales.v1
  market.wallet.v1
  notifier.dlq.v1
)

for topic in "${TOPICS[@]}"; do
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server "$BOOTSTRAP" --create --if-not-exists \
    --topic "$topic" --partitions "$PARTITIONS" --replication-factor 1
done

echo "topics ready:"
/opt/kafka/bin/kafka-topics.sh --bootstrap-server "$BOOTSTRAP" --list
