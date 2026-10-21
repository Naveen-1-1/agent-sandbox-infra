output "run" {
  value = var.run
}

output "region" {
  value = var.region
}

output "cluster_name" {
  value = aws_eks_cluster.this.name
}

output "vpc_id" {
  value = aws_vpc.this.id
}

output "bucket" {
  value = aws_s3_bucket.store.bucket
}

# Images are pushed as <registry>/<name>:<tag>.
output "registry" {
  value = "${data.aws_caller_identity.me.account_id}.dkr.ecr.${var.region}.amazonaws.com/${local.name}"
}

output "node_count" {
  value = var.node_count
}

output "instance_type" {
  value = var.instance_type
}

output "capacity_type" {
  value = var.capacity_type
}
