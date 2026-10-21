# EKS Pod Identity gives two service accounts AWS credentials, with no keys
# stored anywhere: the sbx controller (read and write the bucket, nothing
# else) and the EBS CSI driver (make and attach disks).
data "aws_iam_policy_document" "pods_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "controller" {
  name               = "${local.name}-controller"
  assume_role_policy = data.aws_iam_policy_document.pods_assume.json
}

data "aws_iam_policy_document" "controller_s3" {
  statement {
    sid       = "List"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.store.arn]
  }
  statement {
    sid       = "Objects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${aws_s3_bucket.store.arn}/*"]
  }
}

resource "aws_iam_role_policy" "controller_s3" {
  name   = "s3"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.controller_s3.json
}

resource "aws_eks_pod_identity_association" "controller" {
  cluster_name    = aws_eks_cluster.this.name
  namespace       = var.namespace
  service_account = "sbx-controller"
  role_arn        = aws_iam_role.controller.arn
  depends_on      = [aws_eks_addon.pod_identity]
}

resource "aws_iam_role" "ebs_csi" {
  name               = "${local.name}-ebs-csi"
  assume_role_policy = data.aws_iam_policy_document.pods_assume.json
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}
