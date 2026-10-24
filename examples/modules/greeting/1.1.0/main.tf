# Example module: terraform_data is built into Terraform, so runs need no provider download
# and no cloud credentials. 1.1.0 adds who to greet.
variable "greeting" {
  type    = string
  default = "hello"
}

variable "name" {
  type    = string
  default = "world"
}

resource "terraform_data" "greeting" {
  input = "${var.greeting}, ${var.name}!"
}

output "message" {
  value = terraform_data.greeting.output
}
