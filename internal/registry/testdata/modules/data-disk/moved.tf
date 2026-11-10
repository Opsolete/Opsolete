# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_managed_disk.this
  to   = azurerm_managed_disk.main
}

moved {
  from = azurerm_virtual_machine_data_disk_attachment.this
  to   = azurerm_virtual_machine_data_disk_attachment.main
}
