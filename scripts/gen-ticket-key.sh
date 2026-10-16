#!/usr/bin/env bash
# Creates the local join-ticket signing key (Ed25519, PEM) for the lobby, once (ADR 0005).
# secrets/ticket-private.pem goes only to the lobby; secrets/ticket-public.pem is what game
# servers use to verify tickets (task 2.2). Local development only; AWS keeps the private key in
# Secrets Manager (ADR 0003).
set -euo pipefail

cd "$(dirname "$0")/.."
key=secrets/ticket-private.pem
pub=secrets/ticket-public.pem

mkdir -p secrets
if [[ ! -f "$key" ]]; then
  openssl genpkey -algorithm ed25519 -out "$key" 2>/dev/null || {
    echo "openssl can't create Ed25519 keys; install OpenSSL 3 (brew install openssl)" >&2
    exit 1
  }
  # Readable by the container's non-root user through the bind mount; the directory is gitignored.
  chmod 644 "$key"
  echo "Created $key"
fi
if [[ ! -f "$pub" ]]; then
  openssl pkey -in "$key" -pubout -out "$pub"
  chmod 644 "$pub"
  echo "Created $pub"
fi
