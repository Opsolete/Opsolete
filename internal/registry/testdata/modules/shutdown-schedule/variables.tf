variable "virtual_machine_id" {
  type = string
}

variable "location" {
  type = string
}

variable "time" {
  type        = string
  description = "HHmm, e.g. 1900."
}

variable "timezone" {
  type    = string
  default = "UTC"
}

variable "notification_email" {
  type    = string
  default = null
}

variable "notification_minutes" {
  type    = number
  default = 30
}

variable "existing" {
  type        = bool
  description = "The VM's auto-shutdown schedule exists already (set in the portal, say): import it instead of creating it."
  default     = false
}
