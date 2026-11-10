# A VM's daily auto-shutdown, optionally with an email before it.
resource "azurerm_dev_test_global_vm_shutdown_schedule" "main" {
  virtual_machine_id    = var.virtual_machine_id
  location              = var.location
  enabled               = true
  daily_recurrence_time = var.time
  timezone              = var.timezone

  notification_settings {
    enabled         = var.notification_email != null
    email           = var.notification_email
    time_in_minutes = var.notification_minutes
  }
}
