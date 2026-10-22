# The services (ADR 0003): one ECS service per compose service, with the environment
# infra/docker-compose.yml gives it, pointed at <name>.doodle.internal and the data host.

resource "aws_ecs_cluster" "main" {
  name = "doodle-arena"
  setting {
    name  = "containerInsights"
    value = "disabled" # the free AWS/ECS metrics are enough; Container Insights is billed
  }
}

resource "aws_cloudwatch_log_group" "ecs" {
  name              = "/doodle-arena/ecs"
  retention_in_days = 3
}

locals {
  registry   = "${local.account_id}.dkr.ecr.${var.region}.amazonaws.com/doodle-arena"
  public_url = "https://${var.domain_name}"
  issuer     = "${local.public_url}/api/auth"
  jwks       = "http://auth.doodle.internal:8080/api/auth/.well-known/jwks.json"
  kafka      = "${local.data_host}:9092"
  redis      = "${local.data_host}:6379"

  common_env = {
    SERVICE_VERSION               = var.image_tag
    LOG_LEVEL                     = "info"
    DEPLOYMENT_ENVIRONMENT        = "aws"
    OTEL_EXPORTER_OTLP_ENDPOINT   = "http://${local.data_host}:4318"
    OTEL_EXPORTER_OTLP_PROTOCOL   = "http/protobuf"
    OTEL_SEMCONV_STABILITY_OPT_IN = "http"
    OTEL_METRIC_EXPORT_INTERVAL   = "10000"
  }

  s3_env = {
    S3_ENDPOINT       = "" # AWS's own, with the task role's credentials
    S3_REGION         = var.region
    S3_PATH_STYLE     = "false"
    ROUND_LOGS_BUCKET = aws_s3_bucket.bucket["round-logs"].bucket
  }

  assets_env = merge(local.s3_env, {
    DB_HOST            = local.data_host
    DB_NAME            = "assets"
    DB_USER            = "assets"
    DB_REPLICA_HOST    = local.data_host
    DB_REPLICA_PORT    = "5433"
    REDIS_URL          = "redis://${local.redis}/1"
    REDIS_USERNAME     = "assets"
    AUTH_JWKS_URL      = local.jwks
    JWT_ISSUER         = local.issuer
    PUBLIC_BASE_URL    = local.public_url
    SECURE_COOKIES     = "true"
    S3_PUBLIC_ENDPOINT = ""
    DRAWINGS_BUCKET    = aws_s3_bucket.bucket["drawings"].bucket
    AI_URL             = "http://ai.doodle.internal:8080"
  })
  assets_secrets = {
    DJANGO_SECRET_KEY = local.secret_arn["django-secret-key"]
    DB_PASSWORD       = local.secret_arn["assets-db-password"]
    MONGO_URL         = local.secret_arn["mongo-url"]
    REDIS_PASSWORD    = local.secret_arn["redis-assets-password"]
  }
  # Kafka for assets' round consumer and outbox relay only, as in compose (ADR 0013).
  assets_kafka_env     = merge(local.assets_env, local.kafka_env, { KAFKA_USERNAME = "assets" })
  assets_kafka_secrets = merge(local.assets_secrets, { KAFKA_PASSWORD = local.secret_arn["kafka-assets-password"] })
  auth_db_env          = { DB_HOST = local.data_host, DB_NAME = "auth", DB_USER = "auth" }

  go_health     = ["CMD", "/app", "healthcheck"]
  py_health     = ["CMD", "python", "-c", "import sys, urllib.request; sys.exit(urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2).status != 200)"]
  alive_health  = ["CMD", "python", "-c", "import os, sys, time; sys.exit(time.time() - os.path.getmtime('/tmp/relay-alive') > 30)"]
  consumer_live = ["CMD", "python", "-c", "import os, sys, time; sys.exit(time.time() - os.path.getmtime('/tmp/consumer-alive') > 30)"]

  # name => settings. image: the ECR repository; role: the task role (local.task_roles);
  # discover: other services call it by name.
  services = {
    lobby = {
      image = "lobby", ports = [8080, 9090], discover = true, singleton = true, health = local.go_health
      env = merge(local.kafka_env, {
        GRPC_PORT     = "9090", REDIS_ADDR = local.redis, KAFKA_USERNAME = "lobby",
        AUTH_JWKS_URL = local.jwks, JWT_ISSUER = local.issuer, REDIS_USERNAME = "lobby",
      })
      secrets = {
        TICKET_PRIVATE_KEY_PEM = local.secret_arn["ticket-private-key"]
        KAFKA_PASSWORD         = local.secret_arn["kafka-lobby-password"]
        REDIS_PASSWORD         = local.secret_arn["redis-lobby-password"]
      }
    }
    game-server-1 = {
      image  = "game-server", role = "game-server", ports = [8080, 9090], discover = true, singleton = true,
      health = local.go_health, stop_timeout = 60
      env = merge(local.s3_env, local.kafka_env, {
        GRPC_PORT       = "9090", LOBBY_GRPC_ADDR = "lobby.doodle.internal:9090",
        CONTROL_ADDRESS = "game-server-1.doodle.internal:9090", WS_ADDRESS = "/ws/game-server-1",
        REDIS_ADDR      = local.redis, KAFKA_USERNAME = "game-server", WS_ALLOWED_ORIGINS = var.domain_name,
        REDIS_USERNAME  = "game-server",
      })
      secrets = {
        TICKET_PUBLIC_KEY_PEM = local.secret_arn["ticket-public-key"]
        KAFKA_PASSWORD        = local.secret_arn["kafka-game-server-password"]
        REDIS_PASSWORD        = local.secret_arn["redis-game-server-password"]
      }
    }
    game-server-2 = {
      image  = "game-server", role = "game-server", ports = [8080, 9090], discover = true, singleton = true,
      health = local.go_health, stop_timeout = 60
      env = merge(local.s3_env, local.kafka_env, {
        GRPC_PORT       = "9090", LOBBY_GRPC_ADDR = "lobby.doodle.internal:9090",
        CONTROL_ADDRESS = "game-server-2.doodle.internal:9090", WS_ADDRESS = "/ws/game-server-2",
        REDIS_ADDR      = local.redis, KAFKA_USERNAME = "game-server", WS_ALLOWED_ORIGINS = var.domain_name,
        REDIS_USERNAME  = "game-server",
      })
      secrets = {
        TICKET_PUBLIC_KEY_PEM = local.secret_arn["ticket-public-key"]
        KAFKA_PASSWORD        = local.secret_arn["kafka-game-server-password"]
        REDIS_PASSWORD        = local.secret_arn["redis-game-server-password"]
      }
    }
    notifier = {
      image = "notifier", ports = [8080], discover = true, health = local.go_health
      env = merge(local.kafka_env, {
        DB_HOST           = local.data_host, DB_NAME = "notifier", DB_USER = "notifier",
        AUTH_INTERNAL_URL = "http://auth.doodle.internal:9091", AUTH_JWKS_URL = local.jwks,
        JWT_ISSUER        = local.issuer, KAFKA_USERNAME = "notifier", NOTIFIER_ALLOWED_PRIVATE_HOSTS = "",
      })
      secrets = {
        DB_PASSWORD         = local.secret_arn["notifier-db-password"]
        NOTIFIER_SECRET_KEY = local.secret_arn["notifier-secret-key"]
        KAFKA_PASSWORD      = local.secret_arn["kafka-notifier-password"]
      }
    }
    auth = {
      image = "auth", ports = [8080, 9091], discover = true, health = local.py_health
      env = merge(local.auth_db_env, {
        PUBLIC_BASE_URL     = local.public_url, COOKIE_SECURE = "true", JWT_ISSUER = local.issuer,
        GOOGLE_REDIRECT_URI = "${local.public_url}/api/auth/google/callback",
      })
      secrets = {
        DB_PASSWORD          = local.secret_arn["auth-db-password"]
        JWT_PRIVATE_KEY_PEM  = local.secret_arn["jwt-private-key"]
        OAUTH_STATE_SECRET   = local.secret_arn["oauth-state-secret"]
        GOOGLE_CLIENT_ID     = local.secret_arn["google/client-id"]
        GOOGLE_CLIENT_SECRET = local.secret_arn["google/client-secret"]
      }
    }
    auth-relay = {
      image            = "auth", command = ["python", "-m", "app.relay"], singleton = true, health = local.alive_health
      instance_service = "auth"
      env              = merge(local.auth_db_env, local.kafka_env, { KAFKA_USERNAME = "auth" })
      secrets = {
        DB_PASSWORD    = local.secret_arn["auth-db-password"]
        KAFKA_PASSWORD = local.secret_arn["kafka-auth-password"]
      }
    }
    ai = {
      image = "ai", ports = [8080], discover = true, health = local.py_health
      env   = { MODERATION_MODE = "approve", MODERATION_DELAY_MS = "0", MODERATION_FAILURE_RATE = "0" }
    }
    assets = {
      image = "assets", role = "assets", ports = [8080], discover = true, health = local.py_health,
      cpu   = 512, memory = 1024, env = local.assets_env, secrets = local.assets_secrets
    }
    assets-worker = {
      image   = "assets", role = "assets", cpu = 512, memory = 1024, instance_service = "assets",
      command = ["celery", "-A", "config", "worker", "--hostname", "worker@%h", "--concurrency", "2"],
      health  = ["CMD-SHELL", "celery -A config inspect ping --timeout 5 >/dev/null"],
      env     = local.assets_env, secrets = local.assets_secrets
    }
    assets-consumer = {
      image   = "assets", role = "assets", singleton = true, instance_service = "assets",
      command = ["python", "manage.py", "consume_rounds"], health = local.consumer_live,
      env     = local.assets_kafka_env, secrets = local.assets_kafka_secrets
    }
    assets-relay = {
      image   = "assets", role = "assets", singleton = true, instance_service = "assets",
      command = ["python", "manage.py", "relay_outbox"], health = local.alive_health,
      env     = local.assets_kafka_env, secrets = local.assets_kafka_secrets
    }
    assets-beat = {
      image   = "assets", role = "assets", singleton = true, instance_service = "assets",
      command = ["celery", "-A", "config", "beat", "--schedule", "/tmp/celerybeat-schedule", "--pidfile="],
      health  = ["CMD", "python", "-c", "import glob, os, sys, time; sys.exit(time.time() - max(os.path.getmtime(f) for f in glob.glob('/tmp/celerybeat-schedule*')) > 180)"],
      env     = local.assets_env, secrets = local.assets_secrets
    }
    marketplace = {
      image  = "marketplace", ports = [8080], discover = true, cpu = 512, memory = 1024, start_period = 90,
      health = ["CMD", "curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1:8080/healthz"]
      env = merge(local.kafka_env, {
        DB_HOST         = local.data_host, DB_NAME = "marketplace", DB_USER = "marketplace",
        AUTH_JWKS_URL   = local.jwks, JWT_ISSUER = local.issuer, KAFKA_USERNAME = "marketplace",
        PUBLIC_BASE_URL = local.public_url,
        # The read replica on the data host (ADR 0020), and its cookie behind https.
        DB_REPLICA_HOST = local.data_host, DB_REPLICA_PORT = "5433", DB_REPLICA_COOKIE_SECURE = "true",
      })
      secrets = {
        DB_PASSWORD           = local.secret_arn["marketplace-db-password"]
        KAFKA_PASSWORD        = local.secret_arn["kafka-marketplace-password"]
        STRIPE_SECRET_KEY     = local.secret_arn["stripe/secret-key"]
        STRIPE_WEBHOOK_SECRET = local.secret_arn["stripe/webhook-secret"]
      }
    }
    gateway = {
      image  = "gateway", ports = [80], target_group = true,
      health = ["CMD", "curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1/healthz"]
      env = {
        SERVICE_OTLP_ENDPOINT = "${local.data_host}:4317", SERVICE_ENVIRONMENT = "aws",
        SERVICE_RESOLVER      = "169.254.169.253", SERVICE_DOMAIN = ".doodle.internal",
        SERVICE_WEB_PORT      = "8080", SERVICE_TRUSTED_PROXIES = aws_vpc.main.cidr_block,
        SERVICE_OTLP_BROWSER  = "${local.data_host}:4319",
      }
    }
    web = {
      image  = "web", ports = [8080], discover = true,
      health = ["CMD", "curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1:8080/"]
    }
  }
}

locals {
  # As in compose: <name>-1, except a game server, whose instance id is its server id
  # (game-server-1), the one the gateway routes /ws/<server_id>/ by (ADR 0002).
  instance_id = { for name in keys(local.services) : name => startswith(name, "game-server") ? name : "${name}-1" }
  # A rolling redeploy replaces one game server at a time, after everything else
  # (scripts/aws/up.sh): two stopping together couldn't take each other's rooms, and a gateway
  # or lobby restarting during a drain makes every player reconnect at once.
  image_tag = { for name in keys(local.services) : name => lookup(var.held_game_servers, name, var.image_tag) }
}

# for_each over the names: the services' settings differ in shape (each leaves out what it
# doesn't need), which a map value can't.
module "svc" {
  source   = "../modules/service"
  for_each = toset(keys(local.services))

  name    = each.key
  image   = "${local.registry}/${local.services[each.key].image}:${local.image_tag[each.key]}"
  command = try(local.services[each.key].command, null)
  cpu     = try(local.services[each.key].cpu, 256)
  memory  = try(local.services[each.key].memory, 512)
  ports   = try(local.services[each.key].ports, [])
  # Each process names itself, as in compose (service.instance.id; Python adds a per-process suffix).
  environment = merge(local.common_env, { SERVICE_VERSION = local.image_tag[each.key] }, try(local.services[each.key].env, {}), each.key == "gateway" ? {
    SERVICE_INSTANCE_ID = "gateway-1"
    } : {
    SERVICE_INSTANCE_ID      = local.instance_id[each.key]
    OTEL_SERVICE_NAME        = try(local.services[each.key].instance_service, startswith(each.key, "game-server") ? "game-server" : each.key)
    OTEL_RESOURCE_ATTRIBUTES = "service.instance.id=${local.instance_id[each.key]},service.version=${local.image_tag[each.key]},deployment.environment=aws"
  })
  secrets             = try(local.services[each.key].secrets, {})
  health_check        = try(local.services[each.key].health, null)
  health_start_period = try(local.services[each.key].start_period, 30)
  stop_timeout        = try(local.services[each.key].stop_timeout, 30)
  singleton           = try(local.services[each.key].singleton, false)
  desired_count       = var.running ? 1 : 0
  discoverable        = try(local.services[each.key].discover, false)
  namespace_id        = aws_service_discovery_private_dns_namespace.internal.id
  load_balanced       = try(local.services[each.key].target_group, false)
  target_group_arn    = aws_lb_target_group.gateway.arn

  cluster_id         = aws_ecs_cluster.main.id
  vpc_id             = aws_vpc.main.id
  subnet_ids         = aws_subnet.private[*].id
  execution_role_arn = aws_iam_role.execution.arn
  task_role_arn      = aws_iam_role.task[try(local.services[each.key].role, "default")].arn
  log_group          = aws_cloudwatch_log_group.ecs.name
  region             = var.region

  depends_on = [aws_lb_listener.https]
}

# One-off migration tasks (scripts/aws/up.sh runs them before the services start), in the
# security group of the service whose database they migrate.
resource "aws_ecs_task_definition" "migrate" {
  for_each = {
    auth-migrate = {
      image   = "auth", command = ["alembic", "upgrade", "head"], env = local.auth_db_env,
      secrets = { DB_PASSWORD = local.secret_arn["auth-db-password"] }
    }
    assets-migrate = {
      image   = "assets", command = ["python", "manage.py", "migrate", "--noinput"],
      env     = merge(local.assets_env, { SERVICE_INSTANCE_ID = "assets-migrate", OTEL_SERVICE_NAME = "assets" }),
      secrets = local.assets_secrets
    }
  }

  family                   = "doodle-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task["default"].arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name        = each.key
    image       = "${local.registry}/${each.value.image}:${var.image_tag}"
    essential   = true
    command     = each.value.command
    environment = [for k in sort(keys(merge(local.common_env, each.value.env))) : { name = k, value = merge(local.common_env, each.value.env)[k] }]
    secrets     = [for k in sort(keys(each.value.secrets)) : { name = k, valueFrom = each.value.secrets[k] }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.ecs.name
        awslogs-region        = var.region
        awslogs-stream-prefix = each.key
      }
    }
  }])
}
