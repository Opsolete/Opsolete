terraform {
  required_version = ">= 1.16" # its building blocks import existing parts
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

# Opsolete runs a template as the root module, so the module configures its provider.
# Credentials come from ARM_* env variables (a workspace variable set), never from inputs.
provider "azurerm" {
  features {}
}
