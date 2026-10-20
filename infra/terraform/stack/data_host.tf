# The data host (ADR 0003): Postgres, Redis, MongoDB, Kafka and otel-lgtm in containers on one
# Graviton instance, from infra/aws/data-host/compose.yml. No SSH: Session Manager only.

locals {
  data_host = "data.doodle.internal"
  # SSM parameters the data host reads into its .env (environment variable = parameter).
  data_host_env = {
    POSTGRES_PASSWORD         = "stack/postgres-password"
    AUTH_DB_PASSWORD          = "stack/auth-db-password"
    ASSETS_DB_PASSWORD        = "stack/assets-db-password"
    MARKETPLACE_DB_PASSWORD   = "stack/marketplace-db-password"
    NOTIFIER_DB_PASSWORD      = "stack/notifier-db-password"
    MONGO_ROOT_PASSWORD       = "stack/mongo-root-password"
    MONGO_ASSETS_PASSWORD     = "stack/mongo-assets-password"
    GRAFANA_ADMIN_PASSWORD    = "stack/grafana-admin-password"
    DISCORD_ALERT_WEBHOOK_URL = "discord/alert-webhook-url"
    # Kafka (ADR 0013): kafka-init sets each service's password; kafka-metrics uses its own.
    KAFKA_ADMIN_PASSWORD       = "stack/kafka-admin-password"
    KAFKA_GAME_SERVER_PASSWORD = "stack/kafka-game-server-password"
    KAFKA_LOBBY_PASSWORD       = "stack/kafka-lobby-password"
    KAFKA_ASSETS_PASSWORD      = "stack/kafka-assets-password"
    KAFKA_MARKETPLACE_PASSWORD = "stack/kafka-marketplace-password"
    KAFKA_NOTIFIER_PASSWORD    = "stack/kafka-notifier-password"
    KAFKA_AUTH_PASSWORD        = "stack/kafka-auth-password"
    KAFKA_METRICS_PASSWORD     = "stack/kafka-metrics-password"
    # Redis (task 5.5): one ACL user per service and key prefix.
    REDIS_ADMIN_PASSWORD       = "stack/redis-admin-password"
    REDIS_LOBBY_PASSWORD       = "stack/redis-lobby-password"
    REDIS_GAME_SERVER_PASSWORD = "stack/redis-game-server-password"
    REDIS_ASSETS_PASSWORD      = "stack/redis-assets-password"
  }
  # Read with the instance role too: the broker's TLS key and the CA, written to files at boot.
  data_host_secrets = concat(values(local.data_host_env), ["stack/kafka-server-pem", "stack/kafka-ca-pem"])

  # The bundle the data host unpacks into /opt/doodle: its compose file and everything that
  # file mounts, from the same sources the local stack uses.
  repo = "${path.module}/../../.."
  bundle = merge(
    { for f in fileset("${local.repo}/infra/aws/data-host", "*") : f => "${local.repo}/infra/aws/data-host/${f}" },
    { for f in fileset("${local.repo}/infra/observability", "**") : "observability/${f}" => "${local.repo}/infra/observability/${f}" },
    { for f in fileset("${local.repo}/infra/postgres/init", "*") : "postgres/init/${f}" => "${local.repo}/infra/postgres/init/${f}" },
    { for f in fileset("${local.repo}/infra/kafka", "*") : "kafka/${f}" => "${local.repo}/infra/kafka/${f}" },
    { "redis/start.sh" = "${local.repo}/infra/redis/start.sh" },
    { "mongo/create-users.js" = "${local.repo}/infra/mongo/create-users.js" },
  )
}

data "archive_file" "data_host" {
  type        = "zip"
  output_path = "${path.module}/.terraform/data-host.zip"

  dynamic "source" {
    for_each = local.bundle
    content {
      filename = source.key
      content  = file(source.value)
    }
  }
}

resource "aws_s3_object" "data_host" {
  bucket = aws_s3_bucket.bucket["config"].id
  key    = "data-host-${data.archive_file.data_host.output_md5}.zip"
  source = data.archive_file.data_host.output_path
  etag   = data.archive_file.data_host.output_md5
}

data "aws_ssm_parameter" "al2023_arm64" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

resource "aws_security_group" "data_host" {
  name        = "doodle-data-host"
  description = "The data host: each store port is open to the services that own its data"
  vpc_id      = aws_vpc.main.id

  egress {
    description = "Image pulls (Docker Hub), SSM and S3"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_instance" "data" {
  ami                    = data.aws_ssm_parameter.al2023_arm64.value
  instance_type          = var.data_instance_type
  subnet_id              = aws_subnet.private[0].id
  vpc_security_group_ids = [aws_security_group.data_host.id]
  iam_instance_profile   = aws_iam_instance_profile.data_host.name

  root_block_device {
    volume_type = "gp3"
    volume_size = 40
    encrypted   = true
  }

  # IMDSv2 only, hop limit 1: containers on Docker's bridge networks (the data stores) can't
  # reach the instance role and its secrets. Only otel-lgtm, on the host network, can: Grafana's
  # CloudWatch data source needs it (infra/aws/data-host/compose.yml).
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  user_data = templatefile("${path.module}/data-host.sh.tftpl", {
    region         = var.region
    bundle_bucket  = aws_s3_bucket.bucket["config"].id
    bundle_key     = aws_s3_object.data_host.key
    data_host_name = local.data_host
    env            = local.data_host_env
  })
  # A new bundle (compose file, dashboards, rules) means a new data host.
  user_data_replace_on_change = true

  tags = { Name = "doodle-data-host" }

  # The NAT gateway must exist before the boot script pulls images.
  depends_on = [aws_nat_gateway.main, aws_route_table_association.private]
}
