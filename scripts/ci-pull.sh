#!/usr/bin/env bash
# Pulls the images a CI job is about to need, each with retries, so a Docker Hub hiccup (a 502, a
# TLS timeout) costs a retry here instead of a red test run. It never fails the job: an image it
# couldn't get is pulled again by whatever needs it, which then reports the real error.
#
#   scripts/ci-pull.sh <service>   the images that service's tests start (Testcontainers)
#   scripts/ci-pull.sh stack       the images make up pulls, its Dockerfiles' bases and the checks'
#   scripts/ci-pull.sh --list <service|stack>   print them, pull nothing
#
# The images are found, not listed by hand: a quoted image name whose tag starts with a version
# number with a dot (every pinned tag here does; that also rules out host:port strings such as
# "kafka:9092") in the service's tracked code, plus, for a Go service, in the libs/go test helpers
# it imports. A new test image is therefore covered without editing this file; one it misses only
# loses the retry.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/services.sh

list=false
if [[ "${1:-}" == --list ]]; then
  list=true
  shift
fi
target="${1:?usage: scripts/ci-pull.sh [--list] <service|stack>}"

# [registry/]name[/name]:tag, the tag a version such as 18.6-trixie, v0.206.0 or 4.2.2.
IMAGE='([a-z0-9.-]+/)*[a-z0-9_-]+:v?[0-9]+\.[A-Za-z0-9._-]+'

# Prints the quoted image names in the tracked source files under the given directories.
literals() {
  local d pathspecs=()
  for d in "$@"; do
    pathspecs+=("$d/*.go" "$d/*.py" "$d/*.java" "$d/*.kt")
  done
  git grep -hoE "\"$IMAGE\"" -- "${pathspecs[@]}" | tr -d '"' || true
}

service_images() {
  local s="$1" dir lang pkg
  lang="$(service_lang "$s")"
  dir="$(service_dir "$s")"
  [[ "$lang" == go || "$lang" == python || "$lang" == java ]] || return 0
  literals "$dir"
  if [[ "$lang" == go ]]; then
    # The libs/go test helpers (kafkatest, redistest, ...) the module imports.
    for pkg in $(git grep -hoE 'doodle-arena/libs/go/[a-z]+test"' -- "$dir/*.go" | sed -E 's|.*/libs/go/||; s|"||' | sort -u); do
      literals "libs/go/$pkg"
    done
  fi
}

stack_images() {
  # What compose pulls: every image: line that isn't one of our builds (doodle-arena-*).
  sed -nE 's/^[[:space:]]+image:[[:space:]]*([^[:space:]#]+).*/\1/p' infra/docker-compose.yml | grep -v '^doodle-arena' || true
  # The Dockerfiles' bases, without stage names (FROM deps AS build) and ARG-built names.
  local f not_images
  for f in $(git ls-files '*Dockerfile'); do
    not_images="scratch$(sed -nE 's/^FROM[[:space:]].*[[:space:]]AS[[:space:]]+([A-Za-z0-9_-]+).*/|\1/ip' "$f" | tr -d '\n')"
    sed -nE 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+).*/\2/p' "$f" \
      | grep -v '\$' | grep -vxE "$not_images" || true
  done
  # Images the checks start themselves (k6, curl, ...), quoted or not, in the scripts and Makefile.
  git grep -hoE "(^|[[:space:]=\"'])$IMAGE" -- 'scripts/*' Makefile | sed -E "s/^[[:space:]=\"']//" || true
}

if [[ "$target" == stack ]]; then
  images="$(stack_images | sort -u)"
else
  images="$(service_images "$target" | sort -u)"
fi

if $list; then
  [[ -n "$images" ]] && echo "$images"
  exit 0
fi
if [[ -z "$images" ]]; then
  echo "ci-pull: no images for $target"
  exit 0
fi

pull() {
  local image="$1" attempt out wait="${CI_PULL_WAIT:-5}" # seconds before the second try, doubling
  for attempt in 1 2 3 4 5; do
    if out="$(docker pull --quiet "$image" 2>&1)"; then
      echo "ci-pull: $image"
      return 0
    fi
    echo "ci-pull: $image failed (attempt $attempt of 5): $(printf '%s\n' "$out" | tail -1)" >&2
    [[ $attempt -lt 5 ]] && { sleep "$wait"; wait=$((wait * 2)); }
  done
  echo "ci-pull: giving up on $image; whatever needs it pulls it again" >&2
}

for image in $images; do
  pull "$image"
done
