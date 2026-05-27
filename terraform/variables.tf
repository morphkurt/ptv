variable "aws_region" {
  type    = string
  default = "ap-southeast-2"
}

variable "function_name" {
  type    = string
  default = "ptv-timetable"
}

variable "ptv_dev_id" {
  type      = string
  sensitive = true
}

variable "ptv_api_key" {
  type      = string
  sensitive = true
}
