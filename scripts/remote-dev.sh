#!/usr/bin/env bash
# Runs one service on the Mac against the full stack on the remote dev box (docs/setup.md,
# "Remote dev box (GCP)"); for now the marketplace only:
#   scripts/remote-dev.sh marketplace
# It forwards the VM's gateway, Grafana, collector, Kafka (external listener) and Postgres (primary
# and replica) to the same ports on the Mac, stops the VM's marketplace container, and puts a
# forwarder in its place that sends the gateway's requests back to the Mac through a reverse
# tunnel. Ctrl-C puts the VM's marketplace back. The Mac's own stack must be down: Kafka advertises
# localhost:9094, so that port on the Mac has to be the VM's broker.
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ "${1:-}" != marketplace ]]; then
  echo "usage: scripts/remote-dev.sh marketplace" >&2
  exit 2
fi

HOST="${DEVBOX_HOST:-devbox}"       # the ~/.ssh/config entry (docs/setup.md)
REMOTE_DIR=doodle-arena             # the VM's checkout, in its home directory
PORT=8090                           # the marketplace on the Mac (8080 is the gateway's forward)
TUNNEL_PORT=18090                   # its reverse tunnel on the VM
FORWARDER=doodle-arena-marketplace-remote
FORWARDS="8080 3000 4318 6379 9094 15432 15433"
COMPOSE="docker compose -f infra/docker-compose.yml --env-file .env"

remote() {
  ssh -o BatchMode=yes "$HOST" "cd $REMOTE_DIR && $1"
}

for port in $FORWARDS $PORT; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN > /dev/null 2>&1; then
    echo "Port $port is in use on this Mac. Stop the local stack (make down) and any other tunnel first." >&2
    exit 1
  fi
done

echo "==> Checking the dev box"
if ! remote "sudo sshd -T | grep -qx 'gatewayports clientspecified'"; then
  echo "The dev box's sshd doesn't allow the reverse tunnel: rerun scripts/devbox/setup.sh on it." >&2
  exit 1
fi
if [[ "$(remote "docker inspect -f '{{.State.Running}}' doodle-arena-marketplace-1 2>/dev/null || true")" != true ]] &&
  [[ -z "$(remote "docker ps -aq -f name=^$FORWARDER\$")" ]]; then
  echo "The dev box's stack isn't running: ssh $HOST, then make up there." >&2
  exit 1
fi
# Containers reach the VM through Docker's bridge address, where the reverse tunnel listens
# (only containers can reach it; the GCP firewall admits nothing but IAP's SSH).
bridge_ip="$(remote "docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}'")"

# The marketplace's own credentials, read from the VM's .env into this process only.
env_value() {
  remote "grep -m1 '^$1=' .env | cut -d= -f2-"
}
DB_PASSWORD="$(env_value MARKETPLACE_DB_PASSWORD)"
KAFKA_PASSWORD="$(env_value KAFKA_MARKETPLACE_PASSWORD)"
REDIS_PASSWORD="$(env_value REDIS_MARKETPLACE_PASSWORD)"
STRIPE_SECRET_KEY="$(env_value STRIPE_SECRET_KEY)"
STRIPE_WEBHOOK_SECRET="$(env_value STRIPE_WEBHOOK_SECRET)"

# The same OpenTelemetry agent as the image, cached under build/ (gitignored).
agent_version="$(grep -o 'opentelemetry-javaagent-[0-9.]*\.jar' services/marketplace/Dockerfile | head -1 | sed 's/opentelemetry-javaagent-//; s/\.jar$//')"
agent_sha256="$(grep -o 'checksum=sha256:[0-9a-f]*' services/marketplace/Dockerfile | head -1 | cut -d: -f2)"
agent="services/marketplace/build/otel/opentelemetry-javaagent-$agent_version.jar"
if [[ ! -f "$agent" ]]; then
  echo "==> OpenTelemetry Java agent $agent_version"
  mkdir -p "$(dirname "$agent")"
  curl -fsSL -o "$agent.tmp" \
    "https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/$agent_version/opentelemetry-javaagent-$agent_version.jar"
  echo "$agent_sha256  $agent.tmp" | shasum -a 256 -c - > /dev/null
  mv "$agent.tmp" "$agent"
fi

echo "==> Building the marketplace"
(cd services/marketplace && ./gradlew --quiet bootJar)
jar="$(ls services/marketplace/build/libs/marketplace-*.jar | head -1)"

tunnel_pid=""
app_pid=""
cleanup() {
  trap - EXIT INT TERM HUP
  if [[ -n "$app_pid" ]]; then
    kill "$app_pid" 2> /dev/null || true
    wait "$app_pid" 2> /dev/null || true
  fi
  echo "==> Putting the dev box's marketplace back" 2> /dev/null || true
  remote "docker rm -f $FORWARDER > /dev/null 2>&1; $COMPOSE start marketplace > /dev/null" ||
    echo "Couldn't restore it: ssh $HOST, then make up there." >&2
  if [[ -n "$tunnel_pid" ]]; then
    kill "$tunnel_pid" 2> /dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

echo "==> Tunnels to $HOST"
forward_args=()
for port in $FORWARDS; do
  forward_args+=(-L "127.0.0.1:$port:localhost:$port")
done
ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  "${forward_args[@]}" -R "$bridge_ip:$TUNNEL_PORT:localhost:$PORT" "$HOST" &
tunnel_pid=$!
for _ in $(seq 60); do
  if curl -fsS -o /dev/null http://localhost:8080/healthz 2> /dev/null; then break; fi
  if ! kill -0 "$tunnel_pid" 2> /dev/null; then
    echo "The tunnel failed (see above)." >&2
    exit 1
  fi
  sleep 0.5
done

echo "==> Replacing the dev box's marketplace with a forwarder to this Mac"
gateway_image="$(grep -m1 '^FROM' services/gateway/Dockerfile | awk '{print $2}')"
remote "$COMPOSE stop marketplace > /dev/null && docker rm -f $FORWARDER > /dev/null 2>&1;
  docker run -d --rm --name $FORWARDER --network doodle-arena_default --network-alias marketplace \
    $gateway_image sh -c \"printf 'events {}\nstream { server { listen 8080; proxy_pass $bridge_ip:$TUNNEL_PORT; } }\n' > /etc/nginx/nginx.conf && exec nginx -g 'daemon off;'\" > /dev/null"

echo "==> Starting the marketplace on :$PORT (Ctrl-C stops it and restores the dev box)"
export PORT DB_PASSWORD KAFKA_PASSWORD REDIS_PASSWORD STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET
export DB_HOST=localhost DB_PORT=15432 DB_NAME=marketplace DB_USER=marketplace
export DB_REPLICA_HOST=localhost DB_REPLICA_PORT=15433
export AUTH_JWKS_URL=http://localhost:8080/api/auth/.well-known/jwks.json JWT_ISSUER=http://localhost:8080/api/auth
export KAFKA_BROKERS=localhost:9094 KAFKA_USERNAME=marketplace
export REDIS_HOST=localhost REDIS_PORT=6379 REDIS_USERNAME=marketplace
export PUBLIC_BASE_URL=http://localhost:8080
export SERVICE_VERSION="${SERVICE_VERSION:-0.1.0}" LOG_LEVEL=info DEPLOYMENT_ENVIRONMENT=local
export SERVICE_INSTANCE_ID=marketplace-mac OTEL_SERVICE_NAME=marketplace
export OTEL_RESOURCE_ATTRIBUTES="service.instance.id=marketplace-mac,service.version=$SERVICE_VERSION,deployment.environment=local"
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_SEMCONV_STABILITY_OPT_IN=http OTEL_METRIC_EXPORT_INTERVAL=10000
# The image's JVM flags, with its 512 MB heap fixed explicitly (the Mac has no container limit).
java -javaagent:"$agent" -XX:+UseSerialGC -Xms512m -Xmx512m -XX:SweeperThreshold=50 -jar "$jar" &
app_pid=$!
wait "$app_pid"
