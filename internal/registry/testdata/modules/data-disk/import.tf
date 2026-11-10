# An existing disk (existing = true) is imported instead of created, together with its attachment: the IDs follow
# from the inputs. Once in state, Terraform skips these blocks.
locals {
  subscription = join("/", slice(split("/", var.virtual_machine_id), 0, 3)) # /subscriptions/<id>
}

import {
  for_each = var.existing ? toset(["main"]) : toset([])
  to       = azurerm_managed_disk.main
  id       = "${local.subscription}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Compute/disks/${var.name}"
}

import {
  for_each = var.existing ? toset(["main"]) : toset([])
  to       = azurerm_virtual_machine_data_disk_attachment.main
  id       = "${var.virtual_machine_id}/dataDisks/${var.name}" # azurerm's ID for the attachment
}
