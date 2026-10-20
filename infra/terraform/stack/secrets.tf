# Generated secrets, as SSM SecureStrings under /doodle-arena/stack/ (ADR 0003: standard
# parameters are free). ECS injects them as environment variables; the data host reads its own
# with its instance role. They also sit in this root's state, which is why the state bucket is
# private, encrypted and TLS-only. A new stack gets new passwords and signing keys: nothing in
# AWS outlives a teardown except the bootstrap root.

locals {
  passwords = toset([
    "postgres-password", "auth-db-password", "assets-db-password", "marketplace-db-password",
    "notifier-db-password", "mongo-root-password", "mongo-assets-password", "django-secret-key",
    "oauth-state-secret", "notifier-secret-key", "grafana-admin-password",
    # Kafka (ADR 0013, kafka.tf): the admin super user and one SCRAM user per service.
    "kafka-admin-password", "kafka-game-server-password", "kafka-lobby-password",
    "kafka-assets-password", "kafka-marketplace-password", "kafka-notifier-password",
    "kafka-auth-password", "kafka-metrics-password",
  ])
}

# Letters and digits only: some end up inside URLs (MONGO_URL).
resource "random_password" "secret" {
  for_each = local.passwords
  length   = 40
  special  = false
}

resource "aws_ssm_parameter" "password" {
  for_each = local.passwords
  name     = "/doodle-arena/stack/${each.key}"
  type     = "SecureString"
  value    = random_password.secret[each.key].result
}

# Access tokens (ADR 0001): RSA 2048, like scripts/gen-jwt-key.sh.
resource "tls_private_key" "jwt" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

# Join tickets (ADR 0005): Ed25519, like scripts/gen-ticket-key.sh.
resource "tls_private_key" "ticket" {
  algorithm = "ED25519"
}

resource "aws_ssm_parameter" "pem" {
  for_each = {
    "jwt-private-key"    = tls_private_key.jwt.private_key_pem_pkcs8
    "ticket-private-key" = tls_private_key.ticket.private_key_pem_pkcs8
    "ticket-public-key"  = tls_private_key.ticket.public_key_pem
  }
  name  = "/doodle-arena/stack/${each.key}"
  type  = "SecureString"
  value = each.value
}

resource "aws_ssm_parameter" "mongo_url" {
  name  = "/doodle-arena/stack/mongo-url"
  type  = "SecureString"
  value = "mongodb://assets:${random_password.secret["mongo-assets-password"].result}@${local.data_host}:27017/assets"
}

# The third-party secrets a human puts (bootstrap root): Google, Stripe, Discord.
data "aws_ssm_parameter" "third_party" {
  for_each = toset(["google/client-id", "google/client-secret", "stripe/secret-key", "stripe/webhook-secret", "discord/alert-webhook-url"])
  name     = "/doodle-arena/${each.key}"
}

locals {
  secret_arn = merge(
    { for k, p in aws_ssm_parameter.password : k => p.arn },
    { for k, p in aws_ssm_parameter.pem : k => p.arn },
    { "mongo-url" = aws_ssm_parameter.mongo_url.arn },
    { for k, p in data.aws_ssm_parameter.third_party : k => p.arn },
  )
}
