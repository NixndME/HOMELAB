# VPC Outputs
output "vpc_id" {
  description = "ID of the VPC"
  value       = module.vpc.vpc_id
}

output "public_subnet_ids" {
  description = "IDs of the public subnets"
  value       = module.vpc.public_subnet_ids
}

# EKS Outputs
output "cluster_name" {
  description = "EKS cluster name"
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Endpoint for EKS control plane"
  value       = module.eks.cluster_endpoint
}

output "cluster_security_group_id" {
  description = "Security group ID attached to the EKS cluster"
  value       = module.eks.cluster_security_group_id
}

output "cluster_arn" {
  description = "EKS cluster ARN"
  value       = module.eks.cluster_arn
}

output "cluster_certificate_authority_data" {
  description = "Base64 encoded certificate data required to communicate with the cluster"
  value       = module.eks.cluster_certificate_authority_data
  sensitive   = true
}

output "cluster_version" {
  description = "The Kubernetes server version of the EKS cluster"
  value       = module.eks.cluster_version
}

output "node_group_arn" {
  description = "EKS node group ARN"
  value       = module.eks.node_group_arn
}

output "oidc_issuer_url" {
  description = "The URL on the EKS cluster OIDC Issuer"
  value       = module.eks.oidc_issuer_url
}

output "oidc_provider_arn" {
  description = "ARN of the OIDC Provider for the EKS cluster"
  value       = module.eks.oidc_provider_arn
}

# IAM Role ARNs for Kubernetes Service Accounts
output "aws_load_balancer_controller_role_arn" {
  description = "ARN of the AWS Load Balancer Controller IAM role"
  value       = aws_iam_role.aws_load_balancer_controller.arn
}

output "external_dns_role_arn" {
  description = "ARN of the External DNS IAM role"
  value       = aws_iam_role.external_dns.arn
}

# Kubectl Configuration Command
output "kubectl_config_command" {
  description = "Command to configure kubectl"
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

# Cost Estimation
output "estimated_monthly_cost" {
  description = "Estimated monthly cost breakdown (USD)"
  value = {
    eks_control_plane = "73.00"
    ec2_spot_instances = "15.00 (2x t3.small spot)"
    ebs_storage = "8.00 (40GB total)"
    alb = "18.00 (when created)"
    route53 = "2.00"
    total_estimated = "116.00"
    note = "Actual costs may vary based on usage. ALB cost applies only when ingress is created."
  }
}

# Next Steps
output "next_steps" {
  description = "Commands to run after Terraform apply"
  value = {
    "1_configure_kubectl" = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
    "2_verify_cluster" = "kubectl get nodes"
    "3_install_alb_controller" = "helm repo add eks https://aws.github.io/eks-charts && helm install aws-load-balancer-controller eks/aws-load-balancer-controller -n kube-system --set clusterName=${module.eks.cluster_name} --set serviceAccount.create=true --set serviceAccount.name=aws-load-balancer-controller --set serviceAccount.annotations.\"eks\\.amazonaws\\.com/role-arn\"=${aws_iam_role.aws_load_balancer_controller.arn}"
    "4_install_external_dns" = "Apply external-dns.yaml with the external-dns role ARN"
    "5_deploy_first_app" = "Deploy your first application with Ingress"
  }
}

# Application Examples
output "sample_app_domains" {
  description = "Sample subdomain structure for your applications"
  value = {
    uptime_kuma = "uptime.${var.domain_name}"
    grafana = "grafana.${var.domain_name}"
    passbolt = "passbolt.${var.domain_name}"
    mimir = "mimir.${var.domain_name}"
    loki = "loki.${var.domain_name}"
    immich = "immich.${var.domain_name}"
    notes = "notes.${var.domain_name}"
    url_shortener = "url.${var.domain_name}"
  }
}