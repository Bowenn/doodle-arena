# Public: the app at the domain's apex, with an ACM certificate (Google sign-in needs an https
# redirect URI, and the refresh cookie is Secure). Private: <service>.doodle.internal for every
# ECS service (Cloud Map) and data.doodle.internal for the data host.

data "aws_route53_zone" "public" {
  name         = var.domain_name
  private_zone = false
}

resource "aws_acm_certificate" "app" {
  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "certificate" {
  for_each = {
    for o in aws_acm_certificate.app.domain_validation_options : o.domain_name => o
  }
  zone_id         = data.aws_route53_zone.public.zone_id
  name            = each.value.resource_record_name
  type            = each.value.resource_record_type
  records         = [each.value.resource_record_value]
  ttl             = 60
  allow_overwrite = true # the same record survives a destroy and re-create of the stack
}

resource "aws_acm_certificate_validation" "app" {
  certificate_arn         = aws_acm_certificate.app.arn
  validation_record_fqdns = [for r in aws_route53_record.certificate : r.fqdn]
}

resource "aws_route53_record" "app" {
  zone_id = data.aws_route53_zone.public.zone_id
  name    = var.domain_name
  type    = "A"
  alias {
    name                   = aws_lb.main.dns_name
    zone_id                = aws_lb.main.zone_id
    evaluate_target_health = false
  }
}

resource "aws_service_discovery_private_dns_namespace" "internal" {
  name = "doodle.internal"
  vpc  = aws_vpc.main.id
}

# The namespace's zone belongs to Cloud Map (Route 53 refuses direct records in it), so the data
# host is a Cloud Map instance too: data.doodle.internal -> its private address.
resource "aws_service_discovery_service" "data_host" {
  name = "data"

  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.internal.id
    routing_policy = "MULTIVALUE"
    dns_records {
      type = "A"
      ttl  = 60
    }
  }
  force_destroy = true
}

resource "aws_service_discovery_instance" "data_host" {
  instance_id = aws_instance.data.id
  service_id  = aws_service_discovery_service.data_host.id
  attributes = {
    AWS_INSTANCE_IPV4 = aws_instance.data.private_ip
  }
}
