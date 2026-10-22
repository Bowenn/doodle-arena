# Doodle Arena developer commands. Works with GNU Make 3.81 (the macOS default).
# Pass s=<service> to target one service, for example: make test s=lobby

# --profile '*': down, ps and logs also reach the opt-in services (PROFILES on make up).
COMPOSE := docker compose -f infra/docker-compose.yml --env-file .env --profile '*'

.PHONY: help up down ps logs test lint smoke proto telemetry-check pipeline-check analytics-check wash-check kafka-acl-check data-acl-check backup-check alerts-check loadtest loadtest-smoke loadtest-live loadtest-live-smoke lighthouse aws-bootstrap aws-push aws-up aws-down aws-grafana aws-check

help:
	@echo "make up | down | ps | logs [s=svc] | test [s=svc] | lint [s=svc] | smoke | telemetry-check | pipeline-check | analytics-check | wash-check | kafka-acl-check | data-acl-check | backup-check | alerts-check | loadtest | loadtest-smoke | loadtest-live | loadtest-live-smoke | lighthouse | proto"
	@echo "AWS (ADR 0003): make aws-bootstrap | aws-push | DOMAIN=<domain> make aws-up | aws-check | aws-grafana | aws-down"

up:
	@./scripts/dev-up.sh

down:
	$(COMPOSE) down

ps:
	$(COMPOSE) ps

# s=game-server shows both instances (the compose services are game-server-1 and -2), s=analytics
# Flink's JobManager and TaskManager.
logs:
	$(COMPOSE) logs -f --tail=100 $(if $(filter game-server,$(s)),game-server-1 game-server-2,$(if $(filter assets,$(s)),assets assets-worker assets-consumer assets-relay assets-beat,$(if $(filter auth,$(s)),auth auth-relay,$(if $(filter analytics,$(s)),analytics-jobmanager analytics-taskmanager,$(s)))))

test:
	@./scripts/test.sh $(s)

lint:
	@./scripts/lint.sh $(s)

smoke:
	@./scripts/smoke.sh

telemetry-check:
	@./scripts/telemetry-check.sh

pipeline-check:
	@./scripts/pipeline-check.sh

# The top-guesses Flink job end to end (ADR 0018); needs PROFILES=analytics make up.
analytics-check:
	@./scripts/analytics-check.sh

# Wash trading end to end with real sales, across a restart of Flink (ADR 0018, task 6.5b). In a
# worktree, set ENV_FILE and DRILL_JWT_KEY to the main checkout's.
wash-check:
	@PATH="$$HOME/.docker/bin:$$PATH" uv run --project services/auth --with confluent-kafka --with boto3 \
		--with 'protobuf>=7.36.2,<8' python scripts/drills/wash_trading.py --restart

# The broker refuses what each service must not do (ADR 0013).
kafka-acl-check:
	@./scripts/kafka-acl-check.sh

# Redis and SeaweedFS refuse each service another service's keys and buckets (task 5.5).
data-acl-check:
	@./scripts/data-acl-check.sh

# The AWS backup's dump and restore (ADR 0016) against the local stack, into throwaway stores.
backup-check:
	@./scripts/backup-check.sh

alerts-check:
	@python3 ./scripts/alerts-check.py

# k6, many small rooms (task 5.3). GATEWAY=https://icanfly.click runs it against AWS.
loadtest:
	@./scripts/loadtest.sh full

loadtest-smoke:
	@./scripts/loadtest.sh smoke

# One live room and 5,000 viewers through the relays (task 6.1c, ADR 0017), about 15 minutes.
loadtest-live:
	@./scripts/loadtest.sh live

loadtest-live-smoke:
	@./scripts/loadtest.sh live-smoke

# Lighthouse, mobile, 3 runs (task 5.4): the production web image locally, or
# GATEWAY=https://icanfly.click for the AWS stack.
lighthouse:
	@./scripts/lighthouse.sh

proto:
	@./scripts/proto.sh

# AWS (ADR 0003, docs/setup.md "AWS"). Each session: aws-push, aws-up, aws-check, aws-down.
aws-bootstrap:
	@./scripts/aws/bootstrap.sh

aws-push:
	@./scripts/aws/push-images.sh

aws-up:
	@./scripts/aws/up.sh

aws-down:
	@./scripts/aws/down.sh

aws-grafana:
	@./scripts/aws/grafana.sh

aws-check:
	@./scripts/aws/check.sh
