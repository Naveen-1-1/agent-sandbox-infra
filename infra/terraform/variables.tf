variable "run" {
  description = "Names this environment and tags everything in it (run=<run>), for Cost Explorer and the teardown check."
  type        = string
  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,19}$", var.run))
    error_message = "run must be 1-20 lowercase letters, digits or dashes."
  }
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "kubernetes_version" {
  description = "EKS Kubernetes version; 1.35+ lets workspaces mount the runtime image as a volume."
  type        = string
  default     = "1.35"
}

variable "instance_type" {
  description = "Node instance type: Graviton (arm64) like the images built on Apple silicon, or x86 (m7i-flex.large on the AWS Free plan); images are built to match."
  type        = string
  default     = "m7g.large"
}

variable "node_count" {
  description = "Nodes in the one managed node group."
  type        = number
  default     = 2
  validation {
    condition     = var.node_count >= 2 && var.node_count <= 3
    error_message = "node_count must be 2 or 3."
  }
}

variable "capacity_type" {
  description = "ON_DEMAND, or SPOT for cheaper nodes that can be taken back."
  type        = string
  default     = "ON_DEMAND"
  validation {
    condition     = contains(["ON_DEMAND", "SPOT"], var.capacity_type)
    error_message = "capacity_type must be ON_DEMAND or SPOT."
  }
}

variable "node_disk_gib" {
  description = "Each node's root volume (gp3): images, including workspace images, land here."
  type        = number
  default     = 50
}

variable "api_allowed_cidrs" {
  description = "Addresses allowed to reach the Kubernetes API (make aws-plan sets your own /32)."
  type        = list(string)
}

variable "namespace" {
  description = "Namespace the platform is installed in; its controller gets S3 access."
  type        = string
  default     = "sbx-system"
}
