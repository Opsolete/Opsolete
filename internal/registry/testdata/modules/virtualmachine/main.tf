# A Windows or Linux VM with its network interface, and optionally data disks, a daily auto-shutdown and a
# network security group: each a building block of its own in this repository (its own folder, versions and
# changelog), published to the registry and composed here.

locals {
  # by LUN: unique per VM, so adding or removing one disk leaves the others alone
  data_disks = { for d in var.data_disks : tostring(d.lun) => d }
  vm_id      = coalesce(one(module.windows[*].id), one(module.linux[*].id))
}

module "nic" {
  source                = "opsolete.localhost:8080/terraform-modules-examples/network-interface/azurerm"
  version               = "~> 1.0"
  name                  = coalesce(var.nic_name, "${var.name}-nic")
  resource_group_name   = var.resource_group_name
  location              = var.location
  subnet_id             = var.subnet_id
  ip_configuration_name = var.ip_configuration_name
  public_ip_address_id  = var.public_ip_address_id
}

module "windows" {
  source                   = "opsolete.localhost:8080/terraform-modules-examples/windows-vm/azurerm"
  version                  = "~> 1.0"
  count                    = var.os_type == "windows" ? 1 : 0
  name                     = var.name
  resource_group_name      = var.resource_group_name
  location                 = var.location
  size                     = var.size
  admin_username           = var.admin_username
  admin_password           = var.admin_password
  network_interface_id     = module.nic.id
  boot_diagnostics_enabled = var.boot_diagnostics_enabled
  secure_boot_enabled      = var.secure_boot_enabled
  vtpm_enabled             = var.vtpm_enabled
  os_disk_type             = var.os_disk_type
  image_publisher          = coalesce(var.image_publisher, "MicrosoftWindowsServer")
  image_offer              = coalesce(var.image_offer, "WindowsServer")
  image_sku                = coalesce(var.image_sku, "2022-datacenter-azure-edition")
}

module "linux" {
  source                   = "opsolete.localhost:8080/terraform-modules-examples/linux-vm/azurerm"
  version                  = "~> 1.0"
  count                    = var.os_type == "linux" ? 1 : 0
  name                     = var.name
  resource_group_name      = var.resource_group_name
  location                 = var.location
  size                     = var.size
  admin_username           = var.admin_username
  ssh_public_key           = var.ssh_public_key
  network_interface_id     = module.nic.id
  boot_diagnostics_enabled = var.boot_diagnostics_enabled
  secure_boot_enabled      = var.secure_boot_enabled
  vtpm_enabled             = var.vtpm_enabled
  os_disk_type             = var.os_disk_type
  image_publisher          = coalesce(var.image_publisher, "Canonical")
  image_offer              = coalesce(var.image_offer, "ubuntu-24_04-lts")
  image_sku                = coalesce(var.image_sku, "server")
}

module "data_disk" {
  source              = "opsolete.localhost:8080/terraform-modules-examples/data-disk/azurerm"
  version             = "~> 1.1" # existing
  for_each            = local.data_disks
  name                = coalesce(each.value.name, "${var.name}-data-${each.key}")
  resource_group_name = var.resource_group_name
  location            = var.location
  virtual_machine_id  = local.vm_id
  lun                 = each.value.lun
  size_gb             = each.value.size_gb
  type                = each.value.type
  caching             = each.value.caching
  existing            = each.value.existing
}

module "shutdown_schedule" {
  source               = "opsolete.localhost:8080/terraform-modules-examples/shutdown-schedule/azurerm"
  version              = "~> 1.1" # existing
  count                = var.auto_shutdown_time != null ? 1 : 0
  virtual_machine_id   = local.vm_id
  location             = var.location
  time                 = var.auto_shutdown_time
  timezone             = var.auto_shutdown_timezone
  notification_email   = var.auto_shutdown_notification_email
  notification_minutes = var.auto_shutdown_notification_minutes
  existing             = var.auto_shutdown_existing
}

module "nsg" {
  source               = "opsolete.localhost:8080/terraform-modules-examples/network-security-group/azurerm"
  version              = "~> 1.1" # existing
  count                = var.nsg_name != null ? 1 : 0
  name                 = var.nsg_name
  resource_group_name  = var.resource_group_name
  location             = var.location
  network_interface_id = module.nic.id
  rules                = var.nsg_rules
  existing             = var.nsg_existing
}
