data "aws_caller_identity" "current" {}

resource "aws_s3_bucket" "chunks" {
  bucket = "${var.project_name}-${data.aws_caller_identity.current.account_id}-chunks"
}

resource "aws_s3_bucket" "snapshots" {
  bucket              = "${var.project_name}-${data.aws_caller_identity.current.account_id}-snapshots"
  object_lock_enabled = true
}

resource "aws_s3_bucket_versioning" "chunks" {
  bucket = aws_s3_bucket.chunks.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "chunks" {
  bucket = aws_s3_bucket.chunks.id
  rule {
    id     = "expire-collected-chunk-versions"
    status = "Enabled"
    filter {}
    expiration {
      expired_object_delete_marker = true
    }
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
  depends_on = [aws_s3_bucket_versioning.chunks]
}

resource "aws_s3_bucket_versioning" "snapshots" {
  bucket = aws_s3_bucket.snapshots.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_object_lock_configuration" "snapshots" {
  bucket = aws_s3_bucket.snapshots.id
  rule {
    default_retention {
      mode = "GOVERNANCE"
      days = var.snapshot_retention_days
    }
  }
  depends_on = [aws_s3_bucket_versioning.snapshots]
}

resource "aws_s3_bucket_public_access_block" "chunks" {
  bucket                  = aws_s3_bucket.chunks.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_public_access_block" "snapshots" {
  bucket                  = aws_s3_bucket.snapshots.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "chunks" {
  bucket = aws_s3_bucket.chunks.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "snapshots" {
  bucket = aws_s3_bucket.snapshots.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_policy" "chunks_tls" {
  bucket = aws_s3_bucket.chunks.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.chunks.arn, "${aws_s3_bucket.chunks.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}

resource "aws_s3_bucket_policy" "snapshots_tls" {
  bucket = aws_s3_bucket.snapshots.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyInsecureTransport"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.snapshots.arn, "${aws_s3_bucket.snapshots.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}

resource "aws_kms_key" "volumes" {
  description             = "EdgeVault per-volume envelope encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 7
}

resource "aws_kms_alias" "volumes" {
  name          = "alias/${var.project_name}-volumes"
  target_key_id = aws_kms_key.volumes.key_id
}
