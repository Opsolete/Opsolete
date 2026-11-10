# An existing schedule (existing = true) is imported instead of created. Azure names a VM's auto-shutdown schedule
# shutdown-computevm-<VM name>, in the VM's resource group. Once in state, Terraform skips this block.
locals {
  vm = split("/", var.virtual_machine_id) # "", subscriptions, <id>, resourceGroups, <rg>, providers, Microsoft.Compute, virtualMachines, <name>
}

import {
  for_each = var.existing ? toset(["main"]) : toset([])
  to       = azurerm_dev_test_global_vm_shutdown_schedule.main
  id       = "/subscriptions/${local.vm[2]}/resourceGroups/${local.vm[4]}/providers/Microsoft.DevTestLab/schedules/shutdown-computevm-${local.vm[8]}"
}
