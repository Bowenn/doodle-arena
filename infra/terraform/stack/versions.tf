terraform {
  required_version = "~> 1.16.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.69"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.1"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.7"
    }
  }

  # The bucket comes from backend.hcl, which scripts/aws/up.sh writes from the bootstrap root's
  # output (bucket = "doodle-arena-tfstate-<account id>").
  backend "s3" {
    key          = "stack.tfstate"
    region       = "us-west-2"
    use_lockfile = true
    encrypt      = true
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "doodle-arena"
      ManagedBy = "terraform"
      Root      = "stack"
    }
  }
}
