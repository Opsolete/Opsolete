terraform {
  required_version = ">= 1.16" # import blocks in modules
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}
