variable "region" {
  description = "The deployment region (docs/setup.md: us-west-2)."
  type        = string
  default     = "us-west-2"
}

variable "domain_name" {
  description = "The app's host name: the hosted zone's apex or a name under it, such as doodle-arena.icanfly.click."
  type        = string
}

variable "zone_name" {
  description = "The Route 53 hosted zone that holds domain_name: its apex or a parent, such as icanfly.click. It already exists (the domain's registration made it); scripts/aws/common.sh finds it from DOMAIN."
  type        = string
}

variable "allowed_cidrs" {
  description = "Who may reach the load balancer: everyone by default, since Kafka has users and ACLs (task 5.5, ADR 0013). A list of addresses narrows it, such as [\"203.0.113.7/32\"]."
  type        = list(string)
  default     = ["0.0.0.0/0"]

  validation {
    condition     = length(var.allowed_cidrs) > 0
    error_message = "List at least one address range, or leave the default (everyone)."
  }
}

variable "image_tag" {
  description = "The images' tag in ECR: the git commit scripts/aws/push-images.sh pushed."
  type        = string
}

variable "held_game_servers" {
  description = "Game servers held at their current image tag during a redeploy of a running stack (scripts/aws/up.sh): name to tag. The other services go first, then one game server at a time, so the two never stop together and each drains into the other. Empty: every service gets image_tag."
  type        = map(string)
  default     = {}
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
