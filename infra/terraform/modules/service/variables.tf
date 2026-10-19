variable "name" {
  description = "Service name: the ECS service, its log stream prefix and its DNS name."
  type        = string
}

variable "image" {
  description = "Full image reference (ECR repository URL and tag)."
  type        = string
}

variable "command" {
  description = "Overrides the image's command (null keeps it)."
  type        = list(string)
  default     = null
}

variable "cpu" {
  description = "Fargate CPU units (256 = 0.25 vCPU)."
  type        = number
  default     = 256
}

variable "memory" {
  description = "Fargate memory in MiB."
  type        = number
  default     = 512
}

variable "ports" {
  description = "Container ports; the first is the load balancer's target when there is one."
  type        = list(number)
  default     = []
}

variable "environment" {
  description = "Plain environment variables."
  type        = map(string)
  default     = {}
}

variable "secrets" {
  description = "Environment variables from SSM parameters: name to parameter ARN."
  type        = map(string)
  default     = {}
}

variable "health_check" {
  description = "Container health check command (the compose one), or null."
  type        = list(string)
  default     = null
}

variable "health_start_period" {
  description = "Seconds before failed health checks count."
  type        = number
  default     = 30
}

variable "stop_timeout" {
  description = "Seconds between SIGTERM and SIGKILL (Fargate allows up to 120)."
  type        = number
  default     = 30
}

variable "desired_count" {
  description = "Running tasks: 1, or 0 while the stack is parked."
  type        = number
  default     = 1
}

variable "singleton" {
  description = "Replace the task on deploy instead of running old and new side by side."
  type        = bool
  default     = false
}

variable "namespace_id" {
  description = "Cloud Map namespace to register <name> in, or null for a service nobody calls."
  type        = string
  default     = null
}

variable "target_group_arn" {
  description = "Load balancer target group (the gateway only)."
  type        = string
  default     = null
}

variable "cluster_id" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  type = list(string)
}

variable "execution_role_arn" {
  type = string
}

variable "task_role_arn" {
  type = string
}

variable "log_group" {
  type = string
}

variable "region" {
  type = string
}
