# The only public entry: HTTPS on the load balancer, then the Nginx gateway, which routes as it
# does locally (WebSockets included, ADR 0002). Open to the team's addresses only (var.team_cidrs).

resource "aws_security_group" "alb" {
  name        = "doodle-alb"
  description = "The load balancer: HTTPS (and the redirect from HTTP) from the team"
  vpc_id      = aws_vpc.main.id
}

resource "aws_vpc_security_group_ingress_rule" "alb" {
  for_each          = { for pair in setproduct(var.team_cidrs, [80, 443]) : "${pair[0]}:${pair[1]}" => pair }
  security_group_id = aws_security_group.alb.id
  cidr_ipv4         = each.value[0]
  from_port         = each.value[1]
  to_port           = each.value[1]
  ip_protocol       = "tcp"
  description       = "Team address"
}

resource "aws_vpc_security_group_egress_rule" "alb_to_gateway" {
  security_group_id            = aws_security_group.alb.id
  referenced_security_group_id = module.svc["gateway"].security_group_id
  from_port                    = 80
  to_port                      = 80
  ip_protocol                  = "tcp"
  description                  = "To the gateway"
}

resource "aws_lb" "main" {
  name               = "doodle-arena"
  load_balancer_type = "application"
  subnets            = aws_subnet.public[*].id
  security_groups    = [aws_security_group.alb.id]
  # Game WebSockets ping every 5 s (task 2.3), well inside this.
  idle_timeout               = 120
  drop_invalid_header_fields = true
}

resource "aws_lb_target_group" "gateway" {
  name                 = "doodle-gateway"
  port                 = 80
  protocol             = "HTTP"
  target_type          = "ip"
  vpc_id               = aws_vpc.main.id
  deregistration_delay = 30

  health_check {
    path                = "/healthz"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 10
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.app.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}
