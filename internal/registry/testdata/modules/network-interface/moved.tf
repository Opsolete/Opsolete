# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_network_interface.this
  to   = azurerm_network_interface.main
}
