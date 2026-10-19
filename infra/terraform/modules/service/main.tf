# One ECS service on Fargate (ARM64), as one compose service is locally (ADR 0003): its task
# definition, its own security group (rules are added by the caller, ADR 0006), and a DNS name
# in the private namespace (<name>.doodle.internal) when other services call it.

terraform {
  required_version = "~> 1.16.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.69"
    }
  }
}

resource "aws_security_group" "this" {
  name        = "doodle-${var.name}"
  description = "${var.name}: ingress rules are listed per caller (ADR 0006)"
  vpc_id      = var.vpc_id

  egress {
    description = "Calls out: other services, the data host, AWS APIs and the internet (NAT)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_service_discovery_service" "this" {
  count = var.discoverable ? 1 : 0
  name  = var.name

  dns_config {
    namespace_id   = var.namespace_id
    routing_policy = "MULTIVALUE"
    dns_records {
      type = "A"
      ttl  = 10
    }
  }

  # ECS's own health checks decide which tasks are registered.
  health_check_custom_config {}
  force_destroy = true
}

resource "aws_ecs_task_definition" "this" {
  family                   = "doodle-${var.name}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name         = var.name
    image        = var.image
    essential    = true
    command      = var.command
    stopTimeout  = var.stop_timeout
    portMappings = [for p in var.ports : { containerPort = p, protocol = "tcp" }]
    environment  = [for k in sort(keys(var.environment)) : { name = k, value = var.environment[k] }]
    secrets      = [for k in sort(keys(var.secrets)) : { name = k, valueFrom = var.secrets[k] }]
    healthCheck = var.health_check == null ? null : {
      command     = var.health_check
      interval    = 10
      timeout     = 5
      retries     = 5
      startPeriod = var.health_start_period
    }
    # stdout also goes to CloudWatch Logs: a copy of what Loki gets, for when the collector
    # itself is down.
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = var.log_group
        awslogs-region        = var.region
        awslogs-stream-prefix = var.name
      }
    }
    readonlyRootFilesystem = false
  }])
}

resource "aws_ecs_service" "this" {
  name                   = var.name
  cluster                = var.cluster_id
  task_definition        = aws_ecs_task_definition.this.arn
  desired_count          = var.desired_count
  launch_type            = "FARGATE"
  enable_execute_command = true # ECS Exec for debugging; needs the task role's ssmmessages rights
  propagate_tags         = "SERVICE"

  # A single-instance service (a game server, a relay) is replaced, not doubled: two copies at
  # once would only fight over the same lease or consumer group.
  deployment_minimum_healthy_percent = var.singleton ? 0 : 100
  deployment_maximum_percent         = var.singleton ? 100 : 200

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [aws_security_group.this.id]
    assign_public_ip = false
  }

  dynamic "service_registries" {
    for_each = aws_service_discovery_service.this
    content {
      registry_arn = service_registries.value.arn
    }
  }

  dynamic "load_balancer" {
    for_each = var.load_balanced ? [var.target_group_arn] : []
    content {
      target_group_arn = load_balancer.value
      container_name   = var.name
      container_port   = var.ports[0]
    }
  }

  # The stack can be "parked" (desired 0) by scripts/aws/up.sh; Terraform owns the count.
  wait_for_steady_state = false
}
