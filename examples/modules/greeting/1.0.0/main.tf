# Example module: terraform_data is built into Terraform, so runs need no provider download
# and no cloud credentials.
variable "greeting" {
  type    = string
  default = "hello"
}

resource "terraform_data" "greeting" {
  input = var.greeting
}

output "greeting" {
  value = terraform_data.greeting.output
}
