# ── DynamoDB ──────────────────────────────────────────────────────────────────

resource "aws_dynamodb_table" "departures" {
  name         = "${var.function_name}-departures"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }
}

# ── IAM: allow Lambda to read/write DynamoDB ──────────────────────────────────

resource "aws_iam_role_policy" "lambda_dynamodb" {
  name = "dynamodb-access"
  role = aws_iam_role.lambda.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:Query",
        "dynamodb:GetItem",
      ]
      Resource = aws_dynamodb_table.departures.arn
    }]
  })
}

# ── Lambda env vars ───────────────────────────────────────────────────────────

# Add DYNAMODB_TABLE to the existing Lambda function via a separate env block.
# We use aws_lambda_function_event_invoke_config to avoid re-creating the function.
# Actually we update the function resource in main.tf — done via locals trick below.

# ── EventBridge: trigger collector every 3 minutes ────────────────────────────

resource "aws_cloudwatch_event_rule" "collector" {
  name                = "${var.function_name}-collector"
  description         = "Trigger PTV departure collector every 3 minutes"
  schedule_expression = "rate(3 minutes)"
}

resource "aws_cloudwatch_event_target" "collector" {
  rule = aws_cloudwatch_event_rule.collector.name
  arn  = aws_lambda_function.ptv.arn
  # Empty input triggers the Lambda with an EventBridge envelope (no rawPath)
}

resource "aws_lambda_permission" "eventbridge" {
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ptv.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.collector.arn
}

# ── API Gateway route ─────────────────────────────────────────────────────────

resource "aws_apigatewayv2_route" "history" {
  api_id    = aws_apigatewayv2_api.ptv.id
  route_key = "GET /history"
  target    = "integrations/${aws_apigatewayv2_integration.ptv.id}"
}
