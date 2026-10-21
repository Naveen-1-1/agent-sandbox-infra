# One managed node group of Graviton nodes on Amazon Linux 2023. The launch
# template sets the root disk and carries the tags onto the instances,
# their disks and network interfaces.
data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${local.name}-node"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly", # pull the platform's images
  ])
  role       = aws_iam_role.node.name
  policy_arn = each.value
}

resource "aws_launch_template" "node" {
  name_prefix = "${local.name}-node-"
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = var.node_disk_gib
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  dynamic "tag_specifications" {
    for_each = ["instance", "volume", "network-interface"]
    content {
      resource_type = tag_specifications.value
      tags          = merge(local.tags, { Name = "${local.name}-node" })
    }
  }
}

resource "aws_eks_node_group" "this" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = local.name
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = aws_subnet.public[*].id
  ami_type        = "AL2023_ARM_64_STANDARD"
  capacity_type   = var.capacity_type
  instance_types  = [var.instance_type]

  scaling_config {
    desired_size = var.node_count
    min_size     = var.node_count
    max_size     = var.node_count
  }
  update_config {
    max_unavailable = 1
  }
  launch_template {
    id      = aws_launch_template.node.id
    version = aws_launch_template.node.latest_version
  }

  depends_on = [aws_iam_role_policy_attachment.node, aws_eks_addon.vpc_cni]
}
