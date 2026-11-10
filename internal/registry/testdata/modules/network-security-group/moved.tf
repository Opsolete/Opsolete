# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_network_security_group.this
  to   = azurerm_network_security_group.main
}

moved {
  from = azurerm_network_interface_security_group_association.this
  to   = azurerm_network_interface_security_group_association.main
}
