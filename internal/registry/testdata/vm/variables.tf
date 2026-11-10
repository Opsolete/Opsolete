variable "name" {
  type        = string
  description = "VM name: 1-64 letters, digits or -, not starting or ending with -."

  validation {
    condition     = can(regex("^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?$", var.name))
    error_message = "1-64 letters, digits or -, not starting or ending with -."
  }
}

variable "resource_group_name" {
  type        = string
  description = "Existing resource group, e.g. the name output of the resourcegroup module."
}

variable "location" {
  type        = string
  description = "Azure region, e.g. westeurope. Must match the subnet's region."
  default     = "westeurope"
}

variable "subnet_id" {
  type        = string
  description = "ID of an existing subnet for the VM's network interface."
}

variable "size" {
  type        = string
  description = "VM size, e.g. Standard_B2s."
  default     = "Standard_B2s"
}

variable "admin_username" {
  type        = string
  description = "Admin user. Azure rejects reserved names like admin or root."
  default     = "azureuser"
}

variable "os_type" {
  type        = string
  description = "windows or linux."
  default     = "windows"

  validation {
    condition     = contains(["windows", "linux"], var.os_type)
    error_message = "os_type must be windows or linux."
  }
}

variable "admin_password" {
  type        = string
  description = "Admin password, required for Windows (12-123 chars, Azure complexity rules)."
  default     = null
  sensitive   = true
}

variable "ssh_public_key" {
  type        = string
  description = "OpenSSH public key for the admin user, required for Linux (ssh-rsa, 2048 bits or more)."
  default     = null
}

# Adopting existing VMs (Opsolete "Adopt existing resources"): settings VMs made elsewhere, e.g. in the Azure
# portal, often have differently. Each defaults to what this module always did, so new VMs don't change.

variable "nic_name" {
  type        = string
  description = "Name of the network interface. Default: <name>-nic. For an adopted VM: its NIC's name, e.g. testvm807."
  default     = null
}

variable "ip_configuration_name" {
  type        = string
  description = "Name of the NIC's IP configuration. For an adopted VM made in the portal: usually ipconfig1."
  default     = "internal"
}

variable "public_ip_address_id" {
  type        = string
  description = "ID of an existing public IP for the NIC. Default: none (private IP only)."
  default     = null
}

variable "secure_boot_enabled" {
  type        = bool
  description = "Secure boot (Trusted Launch). Changing it replaces the VM. Default: Azure's (off)."
  default     = null
}

variable "vtpm_enabled" {
  type        = bool
  description = "Virtual TPM (Trusted Launch). Changing it replaces the VM. Default: Azure's (off)."
  default     = null
}

variable "os_disk_type" {
  type        = string
  description = "OS disk storage type: Standard_LRS, StandardSSD_LRS, Premium_LRS, ... Changing it replaces the VM."
  default     = "Standard_LRS"
}

variable "image_publisher" {
  type        = string
  description = "Image publisher. Default: MicrosoftWindowsServer (Windows), Canonical (Linux). Changing it replaces the VM."
  default     = null
}

variable "image_offer" {
  type        = string
  description = "Image offer. Default: WindowsServer (Windows), ubuntu-24_04-lts (Linux). Changing it replaces the VM."
  default     = null
}

variable "image_sku" {
  type        = string
  description = "Image SKU, e.g. 2025-datacenter-core-g2. Default: 2022-datacenter-azure-edition (Windows), server (Linux). Changing it replaces the VM."
  default     = null
}

variable "boot_diagnostics_enabled" {
  type        = bool
  description = "Boot diagnostics with a managed storage account (on for VMs made in the portal)."
  default     = false
}
