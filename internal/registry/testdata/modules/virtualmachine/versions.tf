terraform {
  required_version = ">= 1.5"
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
