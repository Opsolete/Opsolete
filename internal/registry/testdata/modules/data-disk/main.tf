# An empty managed disk, attached to a VM at a LUN.
resource "azurerm_managed_disk" "main" {
  name                 = var.name
  resource_group_name  = var.resource_group_name
  location             = var.location
  storage_account_type = var.type
  create_option        = "Empty"
  disk_size_gb         = var.size_gb
}

resource "azurerm_virtual_machine_data_disk_attachment" "main" {
  managed_disk_id    = azurerm_managed_disk.main.id
  virtual_machine_id = var.virtual_machine_id
  lun                = var.lun
  caching            = var.caching

  lifecycle {
    # How the disk was first attached (Attach, or Empty for a disk made with the VM in the portal): it only
    # matters then, so an imported attachment mustn't plan its replacement.
    ignore_changes = [create_option]
  }
}
