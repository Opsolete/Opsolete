# Renamed: resources are called main (were this), so existing deployments move instead of being replaced.

moved {
  from = azurerm_dev_test_global_vm_shutdown_schedule.this
  to   = azurerm_dev_test_global_vm_shutdown_schedule.main
}
