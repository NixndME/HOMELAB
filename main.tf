# main.tf - Terraform configuration for creating a new VPC and subnets for EKS with NAT

# Provider configuration
provider "aws" {
  region = "ap-south-1"  # region (Mumbai)
}

# Variables (customize as needed)
variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  default     = "10.0.0.0/16"
}

variable "public_subnet_cidrs" {
  description = "CIDR blocks for public subnets"
  default     = ["10.0.1.0/24", "10.0.2.0/24"]
}

variable "private_subnet_cidrs" {
  description = "CIDR blocks for private subnets"
  default     = ["10.0.3.0/24", "10.0.4.0/24"]
}

variable "availability_zones" {
  description = "Availability zones for subnets"
  default     = ["ap-south-1a", "ap-south-1b"]
}

# Create VPC
resource "aws_vpc" "home_lab_eks_vpc" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true  # Required for EKS
  enable_dns_hostnames = true  # Required for EKS
  tags = {
    Name = "HOME-LAB-EKS-VPC"
  }
}

# Create Internet Gateway (for public subnets)
resource "aws_internet_gateway" "home_lab_igw" {
  vpc_id = aws_vpc.home_lab_eks_vpc.id
  tags = {
    Name = "HOME-LAB-EKS-IGW"
  }
}

# Create public subnets
resource "aws_subnet" "public_subnets" {
  count                   = length(var.public_subnet_cidrs)
  vpc_id                  = aws_vpc.home_lab_eks_vpc.id
  cidr_block              = var.public_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = true  # Auto-assign public IPs (for NAT placement and public access if needed)
  tags = {
    Name = "HOME-LAB-EKS-public-${var.availability_zones[count.index]}"
    "kubernetes.io/role/elb" = "1"  # Tag for EKS load balancers
  }
}

# Create private subnets
resource "aws_subnet" "private_subnets" {
  count                   = length(var.private_subnet_cidrs)
  vpc_id                  = aws_vpc.home_lab_eks_vpc.id
  cidr_block              = var.private_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = false
  tags = {
    Name = "HOME-LAB-EKS-private-${var.availability_zones[count.index]}"
    "kubernetes.io/role/internal-elb" = "1"  # Tag for internal load balancers
  }
}

# Create route table for public subnets
resource "aws_route_table" "public_rt" {
  vpc_id = aws_vpc.home_lab_eks_vpc.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.home_lab_igw.id
  }
  tags = {
    Name = "HOME-LAB-EKS-public-rt"
  }
}

# Associate public route table with public subnets
resource "aws_route_table_association" "public_rta" {
  count          = length(aws_subnet.public_subnets)
  subnet_id      = aws_subnet.public_subnets[count.index].id
  route_table_id = aws_route_table.public_rt.id
}

# Create Elastic IP for NAT Gateway
resource "aws_eip" "nat_eip" {
  domain = "vpc"  # VPC domain for EIP
  tags = {
    Name = "HOME-LAB-EKS-NAT-EIP"
  }
}

# Create single NAT Gateway in one public subnet (for cost savings; shared across private subnets)
resource "aws_nat_gateway" "home_lab_nat" {
  allocation_id = aws_eip.nat_eip.id
  subnet_id     = aws_subnet.public_subnets[0].id  # Place in first public subnet (ap-south-1a)
  tags = {
    Name = "HOME-LAB-EKS-NAT"
  }
}

# Create route table for private subnets (route all private subnets to the single NAT)
resource "aws_route_table" "private_rt" {
  vpc_id = aws_vpc.home_lab_eks_vpc.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.home_lab_nat.id
  }
  tags = {
    Name = "HOME-LAB-EKS-private-rt"
  }
}

# Associate private route table with all private subnets
resource "aws_route_table_association" "private_rta" {
  count          = length(aws_subnet.private_subnets)
  subnet_id      = aws_subnet.private_subnets[count.index].id
  route_table_id = aws_route_table.private_rt.id
}

# Outputs (to get IDs after apply)
output "vpc_id" {
  value = aws_vpc.home_lab_eks_vpc.id
}

output "public_subnet_ids" {
  value = aws_subnet.public_subnets[*].id
}

output "private_subnet_ids" {
  value = aws_subnet.private_subnets[*].id
}

output "igw_id" {
  value = aws_internet_gateway.home_lab_igw.id
}