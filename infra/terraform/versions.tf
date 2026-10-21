# The AWS environment for the sbx platform: a small EKS cluster on Graviton
# nodes in public subnets (no NAT gateway, no load balancer), an S3 bucket
# the controller reaches through EKS Pod Identity, and ECR repositories.
# Everything is tagged project=sbx and run=<run> so the bill can be
# filtered and the teardown checked. State stays local (git-ignored).
terraform {
  required_version = ">= 1.10"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.67.0"
    }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = local.tags
  }
}

locals {
  name = "sbx-${var.run}"
  tags = { project = "sbx", run = var.run }
}

data "aws_caller_identity" "me" {}
