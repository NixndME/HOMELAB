# Project Configuration
variable "project" {
  description = "Project name"
  type        = string
  default     = "HOMELAB"
}

variable "env" {
  description = "Environment (DEV, STAGING, PROD)"
  type        = string
  default     = "DEV"
  validation {
    condition     = contains(["DEV", "STAGING", "PROD"], var.env)
    error_message = "Environment must be one of: DEV, STAGING, PROD."
  }
}

variable "owner" {
  description = "Resource owner"
  type        = string
  default     = "ASWATH"
}

variable "region" {
  description = "AWS region"
  type        = string
  default     = "ap-south-1"
}

variable "region_code" {
  description = "Short region code (e.g., MUM for Mumbai)"
  type        = string
  default     = "MUM"
}

# VPC Configuration
variable "vpc_cidr" {
  description = "CIDR block for VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "public_subnet_cidrs" {
  description = "CIDR blocks for public subnets"
  type        = list(string)
  default     = ["10.0.1.0/24", "10.0.2.0/24"]
}

variable "availability_zones" {
  description = "Availability zones"
  type        = list(string)
  default     = ["ap-south-1a", "ap-south-1b"]
}

# EKS Configuration
variable "kubernetes_version" {
  description = "Kubernetes version"
  type        = string
  default     = "1.29"
}

variable "capacity_type" {
  description = "EC2 capacity type (ON_DEMAND or SPOT)"
  type        = string
  default     = "SPOT"
}

variable "instance_types" {
  description = "EC2 instance types for worker nodes"
  type        = list(string)
  default     = ["t3.small", "t3.medium"]
}

variable "desired_size" {
  description = "Desired number of worker nodes"
  type        = number
  default     = 2
}

variable "max_size" {
  description = "Maximum number of worker nodes"
  type        = number
  default     = 4
}

variable "min_size" {
  description = "Minimum number of worker nodes"
  type        = number
  default     = 1
}

variable "disk_size" {
  description = "EBS disk size for worker nodes (GB)"
  type        = number
  default     = 20
}

variable "domain_name" {
  description = "Domain name for applications (e.g., nixndme.com)"
  type        = string
  default     = "nixndme.com"
}

variable "hosted_zone_id" {
  description = "Route53 hosted zone ID for the domain"
  type        = string
  default     = "Z097429735LYF93GXQICV"
}