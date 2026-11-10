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

variable "existing" {
  type        = bool
  description = "The disk exists already (made in the portal, say), attached to this VM at this LUN: import the disk and its attachment instead of creating them."
  default     = false
}
