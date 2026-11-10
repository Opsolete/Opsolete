# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_linux_virtual_machine.this
  to   = azurerm_linux_virtual_machine.main
}
