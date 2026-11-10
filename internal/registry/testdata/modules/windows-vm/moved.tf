# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_windows_virtual_machine.this
  to   = azurerm_windows_virtual_machine.main
}
