# An existing group (existing = true) is imported instead of created, together with its association to the
# network interface: the IDs follow from the inputs. Once in state, Terraform skips these blocks.
locals {
  subscription = join("/", slice(split("/", var.network_interface_id), 0, 3)) # /subscriptions/<id>
  nsg_id       = "${local.subscription}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Network/networkSecurityGroups/${var.name}"
}

import {
  for_each = var.existing ? toset(["main"]) : toset([])
  to       = azurerm_network_security_group.main
  id       = local.nsg_id
}

import {
  for_each = var.existing ? toset(["main"]) : toset([])
  to       = azurerm_network_interface_security_group_association.main
  id       = "${var.network_interface_id}|${local.nsg_id}" # azurerm's ID for the association
}
