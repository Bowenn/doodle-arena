output "security_group_id" {
  description = "The service's own security group: callers are allowed in by rules on it."
  value       = aws_security_group.this.id
}

output "task_definition_arn" {
  value = aws_ecs_task_definition.this.arn
}
