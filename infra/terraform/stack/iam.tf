# Least privilege per role: the execution role pulls images, writes logs and reads secrets;
# each task role holds only what its code calls (S3 for the game servers and assets); the data
# host may read its own secrets and its config bundle, write backups, and read CloudWatch for
# Grafana.

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
    # Flink's checkpoints: written, listed (the restore finds the newest), and old ones deleted.
    analytics = [
      { actions = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], resources = ["${aws_s3_bucket.bucket["analytics-checkpoints"].arn}/*"] },
      { actions = ["s3:ListBucket", "s3:GetBucketLocation"], resources = [aws_s3_bucket.bucket["analytics-checkpoints"].arn] },
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

locals {
  data_host_secret_arns = [for name in local.data_host_secrets :
  "arn:aws:ssm:${var.region}:${local.account_id}:parameter/doodle-arena/${name}"]
}

resource "aws_iam_role_policy" "data_host" {
  name = "data-host"
  role = aws_iam_role.data_host.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter"]
        Resource = local.data_host_secret_arns
      },
      # AmazonSSMManagedInstanceCore (Session Manager) allows ssm:GetParameter(s) on every
      # parameter, which would let otel-lgtm, the one container that can use this role and
      # takes public browser telemetry, read the signing keys and the Stripe key. An explicit
      # Deny outranks that Allow, so the host reads only its own secrets (Stage 5 review).
      {
        Effect      = "Deny"
        Action      = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath", "ssm:GetParameterHistory"]
        NotResource = local.data_host_secret_arns
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${aws_s3_bucket.bucket["config"].arn}/*"]
      },
      # aws-down's dumps (ADR 0016, infra/backup/dump.sh), and nothing else. Write only:
      # otel-lgtm can use this role too, so it must not read a backup back (restores download
      # through presigned URLs the human running aws-up signs). Not the manifest, which makes a
      # backup restorable and holds the SHA-256 each dump must match, and not the drawings: the
      # human's credentials write both, so a host that is broken into can't forge a backup, and
      # a dump it overwrites fails the restore's checksum.
      {
        Effect = "Allow"
        Action = ["s3:PutObject"]
        Resource = [
          "arn:aws:s3:::doodle-arena-backups-${local.account_id}/backups/*/pg-*.dump",
          "arn:aws:s3:::doodle-arena-backups-${local.account_id}/backups/*/mongo-assets.archive",
        ]
      },
    ]
  })
}

resource "aws_iam_instance_profile" "data_host" {
  name = "doodle-data-host"
  role = aws_iam_role.data_host.name
}
