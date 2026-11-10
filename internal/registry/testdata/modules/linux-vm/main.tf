resource "azurerm_linux_virtual_machine" "main" {
  name                  = var.name
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.size
  admin_username        = var.admin_username
  network_interface_ids = [var.network_interface_id]

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
    publisher = var.image_publisher
    offer     = var.image_offer
    sku       = var.image_sku
    version   = "latest"
  }

  lifecycle {
    # Azure reports additional_capabilities on every VM; this module doesn't manage it (see windows-vm).
    ignore_changes = [additional_capabilities]
    precondition {
      condition     = var.ssh_public_key != null
      error_message = "ssh_public_key is required for Linux."
    }
  }
}
