variable "region" {
  description = "The deployment region (docs/setup.md: us-west-2)."
  type        = string
  default     = "us-west-2"
}

variable "domain_name" {
  description = "The public domain, registered in Route 53 (its hosted zone already exists). The app is served at its apex."
  type        = string
}

variable "team_cidrs" {
  description = "Who may reach the load balancer. Team addresses only until task 5.5 adds Kafka users and ACLs (ADR 0003, ADR 0013)."
  type        = list(string)

  validation {
    condition     = length(var.team_cidrs) > 0 && !contains(var.team_cidrs, "0.0.0.0/0")
    error_message = "Open the stack to everyone only after task 5.5 (ADR 0003): list the team's addresses, such as [\"203.0.113.7/32\"]."
  }
}

variable "image_tag" {
  description = "The images' tag in ECR: the git commit scripts/aws/push-images.sh pushed."
  type        = string
}

variable "running" {
  description = "false: everything exists but no service runs (the migrations run first, scripts/aws/up.sh)."
  type        = bool
  default     = true
}

variable "data_instance_type" {
  description = "The data host: Postgres, Redis, MongoDB, Kafka and otel-lgtm share it."
  type        = string
  default     = "t4g.large"
}
