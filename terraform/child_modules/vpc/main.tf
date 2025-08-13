locals {
  prefix = "${var.project}-${var.env}-${var.region_code}"
  # EKS cluster name for tagging
  eks_cluster_name = "${local.prefix}-EKS-01"
  
  common_tags = {
    Project      = var.project
    Environment  = var.env
    Region       = var.region
    RegionCode   = var.region_code
    ManagedBy    = var.managed_by
    Owner        = var.owner
    CreatedOn    = timestamp()
  }
}

# VPC
resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  
  tags = merge(local.common_tags, {
    Name     = "${local.prefix}-VPC-01"
    Resource = "VPC"
    # EKS required tags
    "kubernetes.io/cluster/${local.eks_cluster_name}" = "shared"
  })
}

# Internet Gateway
resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  
  tags = merge(local.common_tags, {
    Name     = "${local.prefix}-IGW-01"
    Resource = "InternetGateway"
  })
}

# Public Subnets (Cost-optimized: No private subnets to avoid NAT Gateway)
resource "aws_subnet" "public" {
  count = length(var.public_subnet_cidrs)
  
  vpc_id                  = aws_vpc.main.id
  cidr_block              = var.public_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = true
  
  tags = merge(local.common_tags, {
    Name     = "${local.prefix}-PUBLIC-SUBNET-0${count.index + 1}"
    Resource = "PublicSubnet"
    Type     = "Public"
    AZ       = var.availability_zones[count.index]
    # EKS tags for both ALB and worker nodes
    "kubernetes.io/cluster/${local.eks_cluster_name}" = "owned"
    "kubernetes.io/role/elb"                          = "1"
  })
}

# Public Route Table
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  
  tags = merge(local.common_tags, {
    Name     = "${local.prefix}-PUBLIC-RT-01"
    Resource = "RouteTable"
    Type     = "Public"
  })
}

# Public Route to Internet Gateway
resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main.id
}

# Associate Public Subnets with Public Route Table
resource "aws_route_table_association" "public" {
  count = length(aws_subnet.public)
  
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}