#!/usr/bin/env bash
# Creates the local JWT signing key (RSA 2048, PEM) for the auth service, once.
# The key is for local development only; AWS keeps its key in Secrets Manager (ADR 0003).
set -euo pipefail

cd "$(dirname "$0")/.."
key=secrets/jwt-private.pem

if [[ -f "$key" ]]; then
  exit 0
fi

mkdir -p secrets
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" 2>/dev/null
# Readable by the container's non-root user through the bind mount; the directory is gitignored.
chmod 644 "$key"
echo "Created $key"
