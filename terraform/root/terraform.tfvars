# Project Configuration
project      = "HOMELAB"
env          = "DEV"
owner        = "ASWATH"
region       = "ap-south-1"
region_code  = "MUM"

# VPC Configuration
vpc_cidr = "10.0.0.0/16"
public_subnet_cidrs = [
  "10.0.1.0/24",  # ap-south-1a
  "10.0.2.0/24"   # ap-south-1b
]
availability_zones = [
  "ap-south-1a",
  "ap-south-1b"
]

# EKS Configuration
kubernetes_version = "1.29"
capacity_type      = "SPOT"           # Use SPOT instances for cost savings
instance_types     = ["t3.small", "t3.medium"]
desired_size       = 2               # Start with 2 nodes
max_size           = 4               # Can scale up to 4 nodes
min_size           = 1               # Can scale down to 1 node
disk_size          = 20              # 20GB EBS disk per node

# Domain Configuration
domain_name      = "nixndme.com"
hosted_zone_id   = "Z097429735LYF93GXQICV"

# Cost Optimization Notes:
# - Using SPOT instances (60-70% cost savings)
# - Public subnets only (no NAT Gateway = $45/month savings)
# - Minimal disk size (20GB)
# - Start with 2 nodes, auto-scale as needed
# - 2 AZs only (sufficient for HA)