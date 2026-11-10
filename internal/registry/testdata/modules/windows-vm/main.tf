resource "azurerm_windows_virtual_machine" "this" {
  name                  = var.name
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.size
  admin_username        = var.admin_username
  admin_password        = var.admin_password
  network_interface_ids = [var.network_interface_id]

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
    publisher = var.image_publisher
    offer     = var.image_offer
    sku       = var.image_sku
    version   = "latest"
  }

  lifecycle {
    # Azure never returns the password: an imported VM has none in state. Azure reports
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
