variable "region" {
  description = "The deployment region (docs/setup.md: us-west-2)."
  type        = string
  default     = "us-west-2"
}

variable "domain_name" {
  description = "The public domain, registered in Route 53 (its hosted zone already exists). The app is served at its apex."
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

variable "game_server_2_image_tag" {
  description = "Holds game-server-2 at this tag while game-server-1 is replaced (scripts/aws/up.sh redeploying a running stack), so the two game servers never stop together and each drains into the other. Empty: image_tag."
  type        = string
  default     = ""
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
