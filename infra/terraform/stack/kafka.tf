# Kafka in AWS (ADR 0013): SASL_SSL on the data host, so the SCRAM handshake and every record are
# encrypted and the clients know they reach the real broker. A private CA of the stack's own
# (like the JWT keys, made here and gone with the stack) signs the broker's certificate; clients
# trust only that CA, which they get in KAFKA_CA_PEM. The users and their ACLs are the local
# ones (infra/kafka/init.sh), with passwords from secrets.tf; dev-tools doesn't exist here.

resource "tls_private_key" "kafka_ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "kafka_ca" {
  private_key_pem       = tls_private_key.kafka_ca.private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 24 * 365
  allowed_uses          = ["cert_signing", "crl_signing"]
  subject {
    common_name  = "Doodle Arena Kafka CA"
    organization = "Doodle Arena"
  }
}

resource "tls_private_key" "kafka_server" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "kafka_server" {
  private_key_pem = tls_private_key.kafka_server.private_key_pem
  # Services reach the broker by the data host's name; kafka-init and the healthcheck on the
  # host itself by the compose name and localhost.
  dns_names = [local.data_host, "kafka", "localhost"]
  subject {
    common_name = local.data_host
  }
}

resource "tls_locally_signed_cert" "kafka_server" {
  cert_request_pem      = tls_cert_request.kafka_server.cert_request_pem
  ca_private_key_pem    = tls_private_key.kafka_ca.private_key_pem
  ca_cert_pem           = tls_self_signed_cert.kafka_ca.cert_pem
  validity_period_hours = 24 * 365
  allowed_uses          = ["digital_signature", "key_encipherment", "server_auth"]
}

# The broker's PEM keystore: its key (PKCS#8, which Kafka requires) and its certificate. The data
# host writes it to a file at boot; the CA certificate isn't secret.
resource "aws_ssm_parameter" "kafka_tls" {
  for_each = {
    "kafka-server-pem" = { type = "SecureString", value = "${tls_private_key.kafka_server.private_key_pem_pkcs8}${tls_locally_signed_cert.kafka_server.cert_pem}" }
    "kafka-ca-pem"     = { type = "String", value = tls_self_signed_cert.kafka_ca.cert_pem }
  }
  name  = "/doodle-arena/stack/${each.key}"
  type  = each.value.type
  value = each.value.value
}

locals {
  # Every Kafka client's connection settings; each adds its own KAFKA_USERNAME and KAFKA_PASSWORD.
  kafka_env = {
    KAFKA_BROKERS = local.kafka
    KAFKA_CA_PEM  = tls_self_signed_cert.kafka_ca.cert_pem
  }
}
