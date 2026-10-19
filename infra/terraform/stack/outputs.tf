output "url" {
  description = "The app."
  value       = local.public_url
}

output "cluster" {
  value = aws_ecs_cluster.main.name
}

output "data_host_instance_id" {
  description = "For Session Manager: a shell, or the Grafana port forward (docs/setup.md, \"AWS\")."
  value       = aws_instance.data.id
}

output "private_subnets" {
  description = "Where scripts/aws/up.sh runs the migration tasks."
  value       = aws_subnet.private[*].id
}

output "migrate_security_groups" {
  description = "Each migration runs in the security group of the service whose database it migrates."
  value = {
    auth-migrate   = module.svc["auth"].security_group_id
    assets-migrate = module.svc["assets"].security_group_id
  }
}

output "services" {
  value = sort(keys(local.services))
}
