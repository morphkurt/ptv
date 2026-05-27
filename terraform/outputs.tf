output "api_url" {
  description = "Base URL for the PTV timetable API"
  value       = aws_apigatewayv2_stage.default.invoke_url
}

output "timetable_endpoint" {
  description = "Full timetable endpoint"
  value       = "${aws_apigatewayv2_stage.default.invoke_url}/timetable"
}

output "spa_url" {
  description = "HTTPS URL for the commute SPA (iOS + browser)"
  value       = "https://${aws_cloudfront_distribution.spa.domain_name}"
}
