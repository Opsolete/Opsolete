variable "name" {
  type = string
}

variable "resource_group_name" {
  type = string
}

variable "location" {
  type = string
}

variable "virtual_machine_id" {
  type = string
}

variable "lun" {
  type = number
}

variable "size_gb" {
  type = number
}

variable "type" {
  type    = string
  default = "Standard_LRS"
}

variable "caching" {
  type    = string
  default = "ReadWrite"
}
