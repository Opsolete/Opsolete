variable "name" {
  type = string
}

variable "resource_group_name" {
  type = string
}

variable "location" {
  type = string
}

variable "subnet_id" {
  type = string
}

variable "ip_configuration_name" {
  type    = string
  default = "internal"
}

variable "public_ip_address_id" {
  type    = string
  default = null
}
