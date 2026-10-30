locals {
  oidc_issuer  = replace(aws_iam_openid_connect_provider.eks.url, "https://", "")
  edge_subject = "system:serviceaccount:edgevault:edgevault-edge"
  gc_subject   = "system:serviceaccount:edgevault:edgevault-gc"
}

resource "aws_iam_role" "edge" {
  name               = "${var.project_name}-edge"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Federated = aws_iam_openid_connect_provider.eks.arn }, Action = "sts:AssumeRoleWithWebIdentity", Condition = { StringEquals = { "${local.oidc_issuer}:aud" = "sts.amazonaws.com", "${local.oidc_issuer}:sub" = local.edge_subject } } }] })
}
resource "aws_iam_role_policy" "edge" {
  name = "${var.project_name}-edge-data-access"
  role = aws_iam_role.edge.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Sid = "ChunkReadWrite", Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject"], Resource = "${aws_s3_bucket.chunks.arn}/*" },
    { Sid = "ProtectedSnapshotWrite", Effect = "Allow", Action = ["s3:PutObject", "s3:PutObjectRetention"], Resource = "${aws_s3_bucket.snapshots.arn}/protected/*" },
    { Sid = "VolumeDataKeys", Effect = "Allow", Action = ["kms:GenerateDataKey", "kms:Decrypt", "kms:DescribeKey"], Resource = aws_kms_key.volumes.arn }
  ] })
}

resource "aws_iam_role" "gc" {
  name               = "${var.project_name}-gc"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Federated = aws_iam_openid_connect_provider.eks.arn }, Action = "sts:AssumeRoleWithWebIdentity", Condition = { StringEquals = { "${local.oidc_issuer}:aud" = "sts.amazonaws.com", "${local.oidc_issuer}:sub" = local.gc_subject } } }] })
}
resource "aws_iam_role_policy" "gc" {
  name   = "${var.project_name}-gc-delete-chunks-only"
  role   = aws_iam_role.gc.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = "s3:DeleteObject", Resource = "${aws_s3_bucket.chunks.arn}/*" }] })
}

resource "aws_ecr_repository" "edgevault" {
  name                 = var.project_name
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration { scan_on_push = true }
  encryption_configuration { encryption_type = "AES256" }
}
resource "aws_ecr_lifecycle_policy" "edgevault" {
  repository = aws_ecr_repository.edgevault.name
  policy     = jsonencode({ rules = [{ rulePriority = 1, description = "Keep the newest 20 images", selection = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 20 }, action = { type = "expire" } }] })
}
