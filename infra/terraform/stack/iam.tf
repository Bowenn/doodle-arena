# Least privilege per role: the execution role pulls images, writes logs and reads secrets;
# each task role holds only what its code calls (S3 for the game servers and assets); the data
# host may read its own secrets and its config bundle, and read CloudWatch for Grafana.

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "doodle-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "execution_secrets" {
  name = "read-secrets"
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ssm:GetParameters"]
      Resource = ["arn:aws:ssm:${var.region}:${local.account_id}:parameter/doodle-arena/*"]
    }]
  })
}

# Every task role: ECS Exec (a shell in a running task, for debugging).
locals {
  task_roles = {
    default = []
    game-server = [
      { actions = ["s3:PutObject"], resources = ["${aws_s3_bucket.bucket["round-logs"].arn}/*"] },
    ]
    assets = [
      { actions = ["s3:GetObject"], resources = ["${aws_s3_bucket.bucket["round-logs"].arn}/*"] },
      { actions = ["s3:GetObject", "s3:PutObject"], resources = ["${aws_s3_bucket.bucket["drawings"].arn}/*"] },
      # HEAD on a missing image must say 404, not 403, so the pipeline uploads it.
      { actions = ["s3:ListBucket"], resources = [aws_s3_bucket.bucket["drawings"].arn, aws_s3_bucket.bucket["round-logs"].arn] },
    ]
  }
}

resource "aws_iam_role" "task" {
  for_each           = local.task_roles
  name               = "doodle-task-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

resource "aws_iam_role_policy" "task" {
  for_each = local.task_roles
  name     = "task"
  role     = aws_iam_role.task[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [{
        Effect   = "Allow"
        Action   = ["ssmmessages:CreateControlChannel", "ssmmessages:CreateDataChannel", "ssmmessages:OpenControlChannel", "ssmmessages:OpenDataChannel"]
        Resource = "*"
      }],
      [for s in each.value : { Effect = "Allow", Action = s.actions, Resource = s.resources }],
    )
  })
}

# ---------------------------------------------------------------- the data host

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "data_host" {
  name               = "doodle-data-host"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

# Session Manager: a shell and the Grafana port forward, with no SSH key or open port.
resource "aws_iam_role_policy_attachment" "data_host_ssm" {
  role       = aws_iam_role.data_host.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# Grafana's CloudWatch data source.
resource "aws_iam_role_policy_attachment" "data_host_cloudwatch" {
  role       = aws_iam_role.data_host.name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchReadOnlyAccess"
}

resource "aws_iam_role_policy" "data_host" {
  name = "data-host"
  role = aws_iam_role.data_host.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = ["ssm:GetParameter"]
        Resource = [for name in local.data_host_secrets :
        "arn:aws:ssm:${var.region}:${local.account_id}:parameter/doodle-arena/${name}"]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${aws_s3_bucket.bucket["config"].arn}/*"]
      },
    ]
  })
}

resource "aws_iam_instance_profile" "data_host" {
  name = "doodle-data-host"
  role = aws_iam_role.data_host.name
}
