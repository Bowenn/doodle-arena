#!/usr/bin/env bash
# Builds every image the stack runs for linux/arm64 (Graviton) and pushes it to ECR, tagged with
# the commit: scripts/aws/up.sh deploys that tag. The tree must be clean, so a tag always means
# the same code (the repositories refuse to overwrite a tag).
#   scripts/aws/push-images.sh [image ...]     (default: all)
source "$(dirname "$0")/common.sh"
require_aws

if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then
  echo "Commit or stash your changes first: the tag must name the code it was built from." >&2
  exit 1
fi
tag="$(git -C "$ROOT" rev-parse --short=12 HEAD)"
registry="$(account_id).dkr.ecr.$AWS_REGION.amazonaws.com"
aws ecr get-login-password | docker login --username AWS --password-stdin "$registry" >/dev/null

# An image's build context and Dockerfile, as in infra/docker-compose.yml (web: its static
# default target, not the dev server).
build_of() {
  case "$1" in
    lobby | game-server | notifier | auth | assets | marketplace) echo ". services/$1/Dockerfile" ;;
    ai | gateway) echo "services/$1 services/$1/Dockerfile" ;;
    web) echo "web web/Dockerfile" ;;
    analytics) echo ". services/analytics/Dockerfile" ;;
    *) echo "unknown image $1" >&2; return 1 ;;
  esac
}
images=("$@")
[[ ${#images[@]} -gt 0 ]] || images=(lobby game-server notifier auth ai assets marketplace gateway web analytics)

for image in "${images[@]}"; do
  read -r context dockerfile <<<"$(build_of "$image")"
  ref="$registry/doodle-arena/$image:$tag"
  if aws ecr describe-images --repository-name "doodle-arena/$image" --image-ids "imageTag=$tag" >/dev/null 2>&1; then
    echo "==> $image: $tag is already in ECR"
    continue
  fi
  echo "==> $image -> $ref"
  docker buildx build --platform linux/arm64 --provenance=false \
    -f "$ROOT/$dockerfile" -t "$ref" --push "$ROOT/$context"
done
echo "Pushed tag $tag. Deploy it: DOMAIN=<domain> make aws-up"
