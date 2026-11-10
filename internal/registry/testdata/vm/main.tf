resource "azurerm_network_interface" "self" {
  name                = coalesce(var.nic_name, "${var.name}-nic")
  resource_group_name = var.resource_group_name
  location            = var.location

  # Private IP only, taken from the subnet's address range.
  ip_configuration {
    name                          = var.ip_configuration_name
    subnet_id                     = var.subnet_id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = var.public_ip_address_id
  }
}

resource "azurerm_windows_virtual_machine" "self" {
  count                 = var.os_type == "windows" ? 1 : 0
  name                  = var.name
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.size
  admin_username        = var.admin_username
  admin_password        = var.admin_password
  network_interface_ids = [azurerm_network_interface.self.id]

  # Managed boot diagnostics (no storage account): on in VMs made in the portal.
  dynamic "boot_diagnostics" {
    for_each = var.boot_diagnostics_enabled ? [1] : []
    content {}
  }

  secure_boot_enabled = var.secure_boot_enabled
  vtpm_enabled        = var.vtpm_enabled

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.os_disk_type
  }

  source_image_reference {
    publisher = coalesce(var.image_publisher, "MicrosoftWindowsServer")
    offer     = coalesce(var.image_offer, "WindowsServer")
    sku       = coalesce(var.image_sku, "2022-datacenter-azure-edition")
    version   = "latest"
  }

  lifecycle {
    # Azure never returns the password: an adopted (imported) VM has none in state. Azure reports
    # additional_capabilities on every VM; this module doesn't manage it, so an import mustn't plan its removal.
    ignore_changes = [admin_password, additional_capabilities]
    precondition {
      condition     = var.admin_password != null
      error_message = "admin_password is required for Windows."
    }
    precondition {
      condition     = length(var.name) <= 15
      error_message = "Windows VM names are at most 15 characters (computer name limit)."
    }
  }
}

resource "azurerm_linux_virtual_machine" "self" {
  count                 = var.os_type == "linux" ? 1 : 0
  name                  = var.name
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.size
  admin_username        = var.admin_username
  network_interface_ids = [azurerm_network_interface.self.id]

  # SSH key only; password login stays disabled (provider default).
  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  # Managed boot diagnostics (no storage account): on in VMs made in the portal.
  dynamic "boot_diagnostics" {
    for_each = var.boot_diagnostics_enabled ? [1] : []
    content {}
  }

  secure_boot_enabled = var.secure_boot_enabled
  vtpm_enabled        = var.vtpm_enabled

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.os_disk_type
  }

  source_image_reference {
    publisher = coalesce(var.image_publisher, "Canonical")
    offer     = coalesce(var.image_offer, "ubuntu-24_04-lts")
    sku       = coalesce(var.image_sku, "server")
    version   = "latest"
  }

  lifecycle {
    # Azure reports additional_capabilities on every VM; this module doesn't manage it (see the Windows VM).
    ignore_changes = [additional_capabilities]
    precondition {
      condition     = var.ssh_public_key != null
      error_message = "ssh_public_key is required for Linux."
    }
  }
}
