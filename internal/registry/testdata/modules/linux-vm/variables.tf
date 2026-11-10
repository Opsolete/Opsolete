variable "name" {
  type = string
}

variable "resource_group_name" {
  type = string
}

variable "location" {
  type = string
}

variable "size" {
  type = string
}

variable "admin_username" {
  type = string
}

variable "network_interface_id" {
  type = string
}

variable "boot_diagnostics_enabled" {
  type    = bool
  default = false
}

variable "secure_boot_enabled" {
  type    = bool
  default = null
}

variable "vtpm_enabled" {
  type    = bool
  default = null
}

variable "os_disk_type" {
  type    = string
  default = "Standard_LRS"
}

variable "image_publisher" {
  type = string
}

variable "image_offer" {
  type = string
}

variable "image_sku" {
  type = string
}

variable "ssh_public_key" {
  type = string
}
