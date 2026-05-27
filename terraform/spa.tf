data "aws_caller_identity" "current" {}

locals {
  spa_src_dir = "${path.module}/../dashboard/app"
}

# ── S3 bucket ─────────────────────────────────────────────────────────────────

resource "aws_s3_bucket" "spa" {
  bucket = "${var.function_name}-spa-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_public_access_block" "spa" {
  bucket                  = aws_s3_bucket.spa.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ── CloudFront ────────────────────────────────────────────────────────────────

resource "aws_cloudfront_origin_access_control" "spa" {
  name                              = "${var.function_name}-spa-oac"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

resource "aws_cloudfront_distribution" "spa" {
  enabled             = true
  default_root_object = "index.html"
  price_class         = "PriceClass_All"

  origin {
    domain_name              = aws_s3_bucket.spa.bucket_regional_domain_name
    origin_id                = "spa-s3"
    origin_access_control_id = aws_cloudfront_origin_access_control.spa.id
  }

  default_cache_behavior {
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]
    target_origin_id       = "spa-s3"
    viewer_protocol_policy = "redirect-to-https"

    forwarded_values {
      query_string = false
      cookies { forward = "none" }
    }

    min_ttl     = 0
    default_ttl = 300
    max_ttl     = 3600
  }

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  viewer_certificate {
    cloudfront_default_certificate = true
  }
}

resource "aws_s3_bucket_policy" "spa" {
  bucket = aws_s3_bucket.spa.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowCloudFront"
      Effect    = "Allow"
      Principal = { Service = "cloudfront.amazonaws.com" }
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.spa.arn}/*"
      Condition = {
        StringEquals = {
          "AWS:SourceArn" = aws_cloudfront_distribution.spa.arn
        }
      }
    }]
  })
}

# ── Upload SPA files ──────────────────────────────────────────────────────────

resource "aws_s3_object" "spa_index" {
  bucket        = aws_s3_bucket.spa.id
  key           = "index.html"
  source        = "${local.spa_src_dir}/index.html"
  content_type  = "text/html; charset=utf-8"
  cache_control = "no-cache"
  etag          = filemd5("${local.spa_src_dir}/index.html")
}

resource "aws_s3_object" "spa_manifest" {
  bucket        = aws_s3_bucket.spa.id
  key           = "manifest.json"
  source        = "${local.spa_src_dir}/manifest.json"
  content_type  = "application/manifest+json"
  cache_control = "public, max-age=86400"
  etag          = filemd5("${local.spa_src_dir}/manifest.json")
}

resource "aws_s3_object" "spa_sw" {
  bucket        = aws_s3_bucket.spa.id
  key           = "sw.js"
  source        = "${local.spa_src_dir}/sw.js"
  content_type  = "application/javascript"
  cache_control = "no-cache"
  etag          = filemd5("${local.spa_src_dir}/sw.js")
}
