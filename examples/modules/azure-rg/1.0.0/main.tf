# A resource group that can import itself (docs/resource-import.md): free, so the real-Azure end-to-end test
# (internal/e2e/azure_test.go) can create, import, change and destroy it. Credentials: ARM_* env variables.
terraform {
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

provider "azurerm" {
  features {}
}

variable "name" {
  type        = string
  description = "Name: the resource group's name."
}

variable "location" {
  type        = string
  description = "Region: the Azure region."
  default     = "westeurope"
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "existing" {
  type        = bool
  description = "The resource group exists already: import it instead of creating it."
  default     = false
}

data "azurerm_subscription" "current" {}

import {
  for_each = var.existing ? toset([var.name]) : toset([])
  to       = azurerm_resource_group.main
  id       = "${data.azurerm_subscription.current.id}/resourceGroups/${var.name}"
}

resource "azurerm_resource_group" "main" {
  name     = var.name
  location = var.location
  tags     = var.tags
}

output "id" {
  value = azurerm_resource_group.main.id
}
