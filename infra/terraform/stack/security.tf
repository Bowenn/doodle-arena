# Who may call whom (ADR 0006, ADR 0003): every service has its own security group, and each
# rule below names the caller. Anything not listed is refused. Like database per service, a
# store's port is open only to the services that own its data (design doc, section 5).

locals {
  sg = { for name, m in module.svc : name => m.security_group_id }

  # [callee, port, caller]: the services' own ports.
  service_rules = concat(
    # The gateway routes every public path and checks /healthz/<service> (make smoke).
    [for svc in ["auth", "lobby", "game-server-1", "game-server-2", "assets", "ai", "marketplace", "notifier", "web"] :
    [svc, 8080, "gateway"]],
    # Access tokens are checked against auth's JWKS (ADR 0001).
    [for caller in ["lobby", "notifier", "assets", "marketplace"] : ["auth", 8080, caller]],
    [
      ["auth", 9091, "notifier"],       # API-key verification (internal port)
      ["ai", 8080, "assets-worker"],    # moderation
      ["lobby", 9090, "game-server-1"], # registry heartbeats (ADR 0006)
      ["lobby", 9090, "game-server-2"],
      ["game-server-1", 9090, "lobby"], # room allocation and moves (ADR 0006)
      ["game-server-2", 9090, "lobby"],
    ],
  )

  # [port, caller] on the data host.
  data_rules = concat(
    [for c in ["auth", "auth-relay", "notifier", "marketplace", "assets", "assets-worker", "assets-consumer", "assets-relay", "assets-beat"] : [5432, c]],
    [for c in ["lobby", "game-server-1", "game-server-2", "assets", "assets-worker", "assets-consumer", "assets-relay", "assets-beat"] : [6379, c]],
    [for c in ["assets", "assets-worker"] : [27017, c]],
    [for c in ["lobby", "game-server-1", "game-server-2", "notifier", "auth-relay", "marketplace", "assets-consumer", "assets-relay", "assets-worker", "assets"] : [9092, c]],
    # Telemetry: OTLP/HTTP from every service, OTLP/gRPC from the gateway's module.
    [for c in keys(local.services) : [4318, c]],
    [[4317, "gateway"]],
  )
}

resource "aws_vpc_security_group_ingress_rule" "service" {
  for_each = { for r in local.service_rules : "${r[2]}->${r[0]}:${r[1]}" => r }

  security_group_id            = local.sg[each.value[0]]
  referenced_security_group_id = local.sg[each.value[2]]
  from_port                    = each.value[1]
  to_port                      = each.value[1]
  ip_protocol                  = "tcp"
  description                  = "from ${each.value[2]}"
}

resource "aws_vpc_security_group_ingress_rule" "gateway_from_alb" {
  security_group_id            = local.sg["gateway"]
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = 80
  to_port                      = 80
  ip_protocol                  = "tcp"
  description                  = "from the load balancer"
}

resource "aws_vpc_security_group_ingress_rule" "data_host" {
  for_each = { for r in local.data_rules : "${r[1]}->data:${r[0]}" => r }

  security_group_id            = aws_security_group.data_host.id
  referenced_security_group_id = local.sg[each.value[1]]
  from_port                    = each.value[0]
  to_port                      = each.value[0]
  ip_protocol                  = "tcp"
  description                  = "from ${each.value[1]}"
}
