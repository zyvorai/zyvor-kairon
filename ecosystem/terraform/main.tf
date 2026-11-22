# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
terraform {
  required_version = ">= 1.5"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.30, < 4.0"
    }
  }
}

variable "manifest" {
  description = "Machine object rendered by ecosystem/catalog.py; namespace must already exist."
  type        = any
  validation {
    condition     = try(var.manifest.kind == "Machine" && var.manifest.apiVersion == "kairon.zyvor.dev/v1", false)
    error_message = "Supply a Kairon Machine manifest."
  }
}

resource "kubernetes_manifest" "machine" {
  manifest = var.manifest
}

output "machine_name" {
  value = kubernetes_manifest.machine.object.metadata.name
}
