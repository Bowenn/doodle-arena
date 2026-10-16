# Doodle Arena developer commands. Works with GNU Make 3.81 (the macOS default).
# Pass s=<service> to target one service, for example: make test s=lobby

COMPOSE := docker compose -f infra/docker-compose.yml --env-file .env

.PHONY: help up down ps logs test lint smoke proto telemetry-check

help:
	@echo "make up | down | ps | logs [s=svc] | test [s=svc] | lint [s=svc] | smoke | telemetry-check | proto"

up:
	@./scripts/dev-up.sh

down:
	$(COMPOSE) down

ps:
	$(COMPOSE) ps

logs:
	$(COMPOSE) logs -f --tail=100 $(s)

test:
	@./scripts/test.sh $(s)

lint:
	@./scripts/lint.sh $(s)

smoke:
	@./scripts/smoke.sh

telemetry-check:
	@./scripts/telemetry-check.sh

proto:
	@./scripts/proto.sh
