output "id" {
  value = local.vm_id
}

output "name" {
  value = var.name
}

output "private_ip_address" {
  value = module.nic.private_ip_address
}

output "data_disk_ids" {
  value = { for k, d in module.data_disk : k => d.id }
}

output "network_security_group_id" {
  value = one(module.nsg[*].id)
}
