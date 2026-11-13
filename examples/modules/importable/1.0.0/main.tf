# Example of a module written for import (docs/resource-import.md): an input `existing` switches on its own
# import block; the id comes from the inputs (a real module finds it with a data source). terraform_data is
# built into Terraform, so it imports offline.
variable "name" {
  type = string
}

variable "size" {
  type    = string
  default = "small"
}

variable "existing" {
  type    = bool
  default = false
}

import {
  for_each = var.existing ? toset([var.name]) : toset([])
  to       = terraform_data.this
  id       = var.name
}

resource "terraform_data" "this" {
  input = var.size
}

output "size" {
  value = terraform_data.this.output
}
