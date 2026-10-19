terraform {
  required_version = "~> 1.16.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.69"
    }
  }

  # State lives in the bucket this root creates. The first run uses local state, then
  # scripts/aws/bootstrap.sh moves it here (terraform init -migrate-state).
  backend "s3" {
    key          = "bootstrap.tfstate"
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
      Root      = "bootstrap"
    }
  }
}
