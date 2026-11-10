# Resources of the module before its building blocks (3.2.0 and older), so existing deployments move instead of
# being replaced. Data disks (3.2.0) are keyed by LUN: move each with
#   terraform state mv 'azurerm_managed_disk.data["0"]' 'module.data_disk["0"].azurerm_managed_disk.main'
# (and the same for azurerm_virtual_machine_data_disk_attachment.data).

moved {
  from = azurerm_network_interface.self
  to   = module.nic.azurerm_network_interface.this
}

moved {
  from = azurerm_windows_virtual_machine.self[0]
  to   = module.windows[0].azurerm_windows_virtual_machine.this
}

moved {
  from = azurerm_linux_virtual_machine.self[0]
  to   = module.linux[0].azurerm_linux_virtual_machine.this
}

moved {
  from = azurerm_dev_test_global_vm_shutdown_schedule.self[0]
  to   = module.shutdown_schedule[0].azurerm_dev_test_global_vm_shutdown_schedule.this
}

moved {
  from = azurerm_network_security_group.self[0]
  to   = module.nsg[0].azurerm_network_security_group.this
}

moved {
  from = azurerm_network_interface_security_group_association.self[0]
  to   = module.nsg[0].azurerm_network_interface_security_group_association.this
}
