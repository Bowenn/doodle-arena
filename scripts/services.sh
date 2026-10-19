#!/usr/bin/env bash
# Shared list of services and their languages. Sourced by test.sh and lint.sh.

GO_SERVICES=(lobby game-server notifier)
PY_SERVICES=(auth ai assets)
JAVA_SERVICES=(marketplace)
# Not services, but checked by make test and make lint too.
EXTRA_MODULES=(proto eventctl ci secrets observability)
ALL_SERVICES=("${GO_SERVICES[@]}" "${PY_SERVICES[@]}" "${JAVA_SERVICES[@]}" web "${EXTRA_MODULES[@]}")

# Prints go, python, java or web for a service name; fails for unknown names.
service_lang() {
  local s="$1" x
  for x in "${GO_SERVICES[@]}" eventctl; do [[ "$x" == "$s" ]] && { echo go; return; }; done
  for x in "${PY_SERVICES[@]}"; do [[ "$x" == "$s" ]] && { echo python; return; }; done
  for x in "${JAVA_SERVICES[@]}"; do [[ "$x" == "$s" ]] && { echo java; return; }; done
  [[ "$s" == web ]] && { echo web; return; }
  [[ "$s" == proto ]] && { echo proto; return; }
  [[ "$s" == ci ]] && { echo ci; return; }
  [[ "$s" == secrets ]] && { echo secrets; return; }
  [[ "$s" == observability ]] && { echo observability; return; }
  echo "unknown service: $s (known: ${ALL_SERVICES[*]})" >&2
  return 1
}

# Prints the source directory of a service.
service_dir() {
  case "$1" in
    web) echo web ;;
    proto) echo proto ;;
    eventctl) echo tools/eventctl ;;
    ci) echo .github ;;
    secrets) echo . ;;
    observability) echo infra/observability ;;
    *) echo "services/$1" ;;
  esac
}
