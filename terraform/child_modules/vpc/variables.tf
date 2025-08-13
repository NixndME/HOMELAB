variable "project" {
  description = "Project name (e.g., HOMELAB)"
  type        = string
  validation {
    condition     = length(var.project) >= 2
    error_message = "Project name must be at least 2 characters long."
  }
}

variable "env" {
  description = "Environment (DEV, STAGING, PROD)"
  type        = string
  validation {
    condition     = contains(["DEV", "STAGING", "PROD"], var.env)
    error_message = "Environment must be one of: DEV, STAGING, PROD."
  }
}

variable "region" {
  description = "AWS region"
  type        = string
  default     = "ap-south-1"
}

variable "region_code" {
  description = "Region code (e.g., MUM for Mumbai)"
  type        = string
  validation {
    condition     = length(var.region_code) >= 2 && length(var.region_code) <= 5
    error_message = "Region code must be between 2 and 5 characters."
  }
}

variable "managed_by" {
  description = "Who manages these resources"
  type        = string
  default     = "Terraform"
}

variable "owner" {
  description = "Resource owner"
  type        = string
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0))
    error_message = "VPC CIDR must be a valid IPv4 CIDR block."
  }
}

variable "public_subnet_cidrs" {
  description = "List of CIDR blocks for public subnets"
  type        = list(string)
  validation {
    condition     = length(var.public_subnet_cidrs) >= 2 && length(var.public_subnet_cidrs) <= 4
    error_message = "Must provide between 2 and 4 public subnet CIDR blocks."
  }
}

variable "availability_zones" {
  description = "List of availability zones"
  type        = list(string)
  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "Must provide at least 2 availability zones for EKS."
  }
}