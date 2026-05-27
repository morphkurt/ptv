terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

locals {
  lambda_src_dir = "${path.module}/../lambda"
  zip_path       = "${path.module}/../lambda/lambda.zip"
}

# ── Build ─────────────────────────────────────────────────────────────────────

# Rebuild whenever any Go source file changes
resource "null_resource" "build" {
  triggers = {
    src_hash = sha1(join("", [
      filesha1("${local.lambda_src_dir}/main.go"),
      filesha1("${local.lambda_src_dir}/ptv_client.go"),
      filesha1("${local.lambda_src_dir}/journey.go"),
      filesha1("${local.lambda_src_dir}/travel_times.go"),
      filesha1("${local.lambda_src_dir}/collector.go"),
      filesha1("${local.lambda_src_dir}/history.go"),
    ]))
  }

  provisioner "local-exec" {
    command     = "GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap . && zip -q lambda.zip bootstrap && rm bootstrap"
    working_dir = local.lambda_src_dir
  }
}

# ── IAM ───────────────────────────────────────────────────────────────────────

resource "aws_iam_role" "lambda" {
  name = "${var.function_name}-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "basic_execution" {
  role       = aws_iam_role.lambda.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# ── Lambda ────────────────────────────────────────────────────────────────────

resource "aws_lambda_function" "ptv" {
  function_name    = var.function_name
  role             = aws_iam_role.lambda.arn
  filename         = local.zip_path
  source_code_hash = null_resource.build.triggers.src_hash
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 30

  environment {
    variables = {
      PTV_DEV_ID      = var.ptv_dev_id
      PTV_API_KEY     = var.ptv_api_key
      DYNAMODB_TABLE  = "${var.function_name}-departures"
    }
  }

  depends_on = [null_resource.build]
}

resource "aws_cloudwatch_log_group" "ptv" {
  name              = "/aws/lambda/${var.function_name}"
  retention_in_days = 7
}

# ── API Gateway v2 (HTTP API) ─────────────────────────────────────────────────

resource "aws_apigatewayv2_api" "ptv" {
  name          = "${var.function_name}-api"
  protocol_type = "HTTP"
}

resource "aws_apigatewayv2_integration" "ptv" {
  api_id                 = aws_apigatewayv2_api.ptv.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.ptv.invoke_arn
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_route" "timetable" {
  api_id    = aws_apigatewayv2_api.ptv.id
  route_key = "GET /timetable"
  target    = "integrations/${aws_apigatewayv2_integration.ptv.id}"
}

resource "aws_apigatewayv2_route" "travel_times" {
  api_id    = aws_apigatewayv2_api.ptv.id
  route_key = "GET /travel-times"
  target    = "integrations/${aws_apigatewayv2_integration.ptv.id}"
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.ptv.id
  name        = "$default"
  auto_deploy = true

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw.arn
    format = jsonencode({
      requestId      = "$context.requestId"
      ip             = "$context.identity.sourceIp"
      requestTime    = "$context.requestTime"
      httpMethod     = "$context.httpMethod"
      routeKey       = "$context.routeKey"
      status         = "$context.status"
      responseLength = "$context.responseLength"
      integrationError = "$context.integrationErrorMessage"
    })
  }
}

resource "aws_cloudwatch_log_group" "apigw" {
  name              = "/aws/apigateway/${var.function_name}"
  retention_in_days = 7
}

resource "aws_lambda_permission" "apigw" {
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ptv.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.ptv.execution_arn}/*/*"
}
