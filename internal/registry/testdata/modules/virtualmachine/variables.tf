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
  description = "Resource group: an existing one, e.g. the name output of the resourcegroup module."
}

variable "location" {
  type        = string
  description = "Region: e.g. westeurope; must match the subnet's region."
  default     = "westeurope"
}

variable "subnet_id" {
  type        = string
  description = "Subnet ID: an existing subnet for the VM's network interface."
}

variable "size" {
  type        = string
  description = "VM size: e.g. Standard_B2s."
  default     = "Standard_B2s"
}

variable "admin_username" {
  type        = string
  description = "Admin user: Azure rejects reserved names like admin or root."
  default     = "azureuser"
}

variable "os_type" {
  type        = string
  description = "Operating system: windows or linux."
  default     = "windows"

  validation {
    condition     = contains(["windows", "linux"], var.os_type)
    error_message = "os_type must be windows or linux."
  }
}

variable "admin_password" {
  type        = string
  description = "Admin password: required for Windows (12-123 characters, Azure complexity rules)."
  default     = null
  sensitive   = true
}

variable "ssh_public_key" {
  type        = string
  description = "SSH public key: OpenSSH key of the admin user, required for Linux (ssh-rsa, 2048 bits or more)."
  default     = null
}

# Importing existing VMs (Opsolete's Import): settings VMs made elsewhere, e.g. in the Azure portal, often have
# differently. Each defaults to what this module always did, so new VMs don't change.

variable "nic_name" {
  type        = string
  description = "Network interface name: default <name>-nic; for an imported VM its network interface's name, e.g. testvm807."
  default     = null
}

variable "ip_configuration_name" {
  type        = string
  description = "IP configuration name: for an imported VM made in the portal usually ipconfig1."
  default     = "internal"
}

variable "public_ip_address_id" {
  type        = string
  description = "Public IP ID: an existing public IP for the network interface; default none (private IP only)."
  default     = null
}

variable "secure_boot_enabled" {
  type        = bool
  description = "Secure boot: Trusted Launch; changing it replaces the VM. Default off."
  default     = null
}

variable "vtpm_enabled" {
  type        = bool
  description = "Virtual TPM: Trusted Launch; changing it replaces the VM. Default off."
  default     = null
}

variable "os_disk_type" {
  type        = string
  description = "OS disk type: Standard_LRS, StandardSSD_LRS, Premium_LRS, …; changing it replaces the VM."
  default     = "Standard_LRS"
}

variable "image_publisher" {
  type        = string
  description = "Image publisher: default MicrosoftWindowsServer (Windows), Canonical (Linux); changing it replaces the VM."
  default     = null
}

variable "image_offer" {
  type        = string
  description = "Image offer: default WindowsServer (Windows), ubuntu-24_04-lts (Linux); changing it replaces the VM."
  default     = null
}

variable "image_sku" {
  type        = string
  description = "Image SKU: e.g. 2025-datacenter-core-g2; default 2022-datacenter-azure-edition (Windows), server (Linux); changing it replaces the VM."
  default     = null
}

variable "boot_diagnostics_enabled" {
  type        = bool
  description = "Boot diagnostics: with a managed storage account (on for VMs made in the portal)."
  default     = false
}

# Data disks, auto-shutdown and network security group: all optional, none by default.

variable "data_disks_json" {
  type        = string
  description = "Data disks: a JSON list, one object per disk, {\"lun\": 0, \"size_gb\": 64} plus optionally \"name\" (default <name>-data-<lun>), \"type\" (Standard_LRS, StandardSSD_LRS, Premium_LRS, …; default Standard_LRS) and \"caching\" (None, ReadOnly, ReadWrite; default ReadWrite). Default: none."
  default     = "[]"

  validation {
    condition = can([for d in jsondecode(var.data_disks_json) : [d.lun, d.size_gb]]) && length(distinct([
    for d in jsondecode(var.data_disks_json) : d.lun])) == length(jsondecode(var.data_disks_json))
    error_message = "A JSON list of disks, each with a number lun (unique) and size_gb, e.g. [{\"lun\": 0, \"size_gb\": 64}]."
  }
}

variable "auto_shutdown_time" {
  type        = string
  description = "Auto-shutdown time: shut the VM down daily at this time, HHmm, e.g. 1900. Default: no auto-shutdown."
  default     = null

  validation {
    condition     = var.auto_shutdown_time == null || can(regex("^([01][0-9]|2[0-3])[0-5][0-9]$", var.auto_shutdown_time))
    error_message = "HHmm, e.g. 1900."
  }
}

variable "auto_shutdown_timezone" {
  type        = string
  description = "Auto-shutdown time zone: a Windows time zone name, e.g. W. Europe Standard Time."
  default     = "UTC"
}

variable "auto_shutdown_notification_email" {
  type        = string
  description = "Auto-shutdown email: who gets an email before the shutdown. Default: no email."
  default     = null
}

variable "auto_shutdown_notification_minutes" {
  type        = number
  description = "Auto-shutdown email lead time: minutes before the shutdown, 15-120."
  default     = 30
}

variable "nsg_name" {
  type        = string
  description = "Network security group: name of a network security group for the VM's network interface. Default: none."
  default     = null
}

variable "nsg_rules_json" {
  type        = string
  description = "Network security group rules: a JSON list, one object per rule, {\"name\": \"RDP\", \"priority\": 300, \"destination_port_range\": \"3389\"} plus optionally direction (Inbound), access (Allow), protocol (Tcp), source_port_range, source_address_prefix, destination_address_prefix (each *) and description. Default: none."
  default     = "[]"

  validation {
    condition     = can([for r in jsondecode(var.nsg_rules_json) : [r.name, r.priority]])
    error_message = "A JSON list of rules, each with a name and a priority."
  }
}
