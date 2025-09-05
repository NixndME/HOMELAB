# AWS EKS Infrastructure with Terraform, Traefik, and Glance

This repository contains a set of configurations to deploy a complete and functional environment on Amazon EKS (Elastic Kubernetes Service). It uses Terraform for networking, `eksctl` for the EKS cluster, Traefik as the ingress controller, and includes a sample "Glance" dashboard application.

## Components

- **Terraform (`Terraform-VPC`)**: Creates the foundational networking infrastructure, including a VPC, public and private subnets, an Internet Gateway, and a NAT Gateway.
- **EKS (`EKS`)**: Defines the EKS cluster using `eksctl`, provides a script for generating a `kubeconfig`, and includes instructions for installing the AWS Load Balancer Controller.
- **Traefik (`Traefik`)**: Contains all the necessary Kubernetes manifests to deploy Traefik as an ingress controller, exposed to the internet via an AWS Application Load Balancer.
- **Glance (`Glance`)**: A sample Kubernetes application that deploys a "Glance" dashboard, which is a customized portal with links to various resources.

This repository is designed to be used in a step-by-step manner to bring up the full environment. The following sections provide a detailed guide for each component.

## 1. Create the Networking Infrastructure with Terraform

The first step is to create the VPC and related networking resources using Terraform.

1.  **Navigate to the `Terraform-VPC` directory:**
    ```bash
    cd Terraform-VPC
    ```

2.  **Initialize Terraform:**
    ```bash
    terraform init
    ```

3.  **Review the plan:**
    ```bash
    terraform plan
    ```

4.  **Apply the configuration:**
    ```bash
    terraform apply
    ```

    When prompted, type `yes` to confirm the changes.

5.  **Save the output:**
    After the apply is complete, Terraform will output the IDs of the created resources (VPC, subnets, etc.). **Save these values**, as you will need them for the next step.

## 2. Create the EKS Cluster

Once the networking is in place, you can create the EKS cluster.

1.  **Update `EKS/EKSCTL.yaml`:**
    Open the `EKS/EKSCTL.yaml` file and replace the placeholder VPC and subnet IDs with the values you saved from the Terraform output.

    ```yaml
    vpc:
      id: "vpc-0b9b782aa111c0bde"  # <-- Replace with your VPC ID
      subnets:
        public:
          ap-south-1a:
            id: "subnet-04027f0848e3d6ddc"  # <-- Replace with your public subnet ID
          ap-south-1b:
            id: "subnet-054ec56f89ed69000"  # <-- Replace with your public subnet ID
        private:
          ap-south-1a:
            id: "subnet-0aebdc9e65178d534"  # <-- Replace with your private subnet ID
          ap-south-1b:
            id: "subnet-080ce0a4f33fbcc99"  # <-- Replace with your private subnet ID
    ```

2.  **Create the cluster:**
    Use `eksctl` to create the cluster from the configuration file.
    ```bash
    eksctl create cluster -f EKS/EKSCTL.yaml
    ```
    This process can take 15-20 minutes.

3.  **Generate a static kubeconfig (optional):**
    The `EKS/kubeconfig.sh` script can be used to create a static, token-based kubeconfig file for accessing the cluster.
    ```bash
    ./EKS/kubeconfig.sh
    ```

4.  **Install the AWS Load Balancer Controller:**
    Follow the instructions in `EKS/EKS-Loadbalancer.txt` to install the AWS Load Balancer Controller using Helm. This is required for exposing services via an ALB.
    ```bash
    helm repo add eks https://aws.github.io/eks-charts
    helm repo update
    helm install aws-load-balancer-controller eks/aws-load-balancer-controller \
      -n kube-system \
      --set clusterName=home-lab-eks \
      --set serviceAccount.create=true \
      --set serviceAccount.name=aws-load-balancer-controller
    ```

## 3. Deploy Traefik Ingress Controller

With the cluster running, you can now deploy Traefik.

1.  **Apply the Traefik CRDs:**
    It is recommended to apply the CRDs from the official Traefik source.
    ```bash
    kubectl apply -f https://raw.githubusercontent.com/traefik/traefik/v3.5/docs/content/reference/dynamic-configuration/kubernetes-crd-definition-v1.yml
    ```

2.  **Deploy Traefik:**
    Apply the deployment, service, and RBAC configurations.
    ```bash
    kubectl apply -f Traefik/traefik-deployment.yaml
    ```

3.  **Expose Traefik via ALB:**
    Apply the Ingress object that uses the AWS Load Balancer Controller to create an ALB.
    ```bash
    kubectl apply -f Traefik/traefik-alb-ingress.yaml
    ```
    It may take a few minutes for the ALB to be provisioned. You can check the status with `kubectl get ingress traefik-ext-alb-ingress`.

## 4. Deploy the Glance Application

Finally, you can deploy the sample Glance dashboard application.

1.  **Create the `glance` namespace:**
    ```bash
    kubectl create namespace glance
    ```

2.  **Apply the Glance manifest:**
    This will create the deployment, service, PVC, and ingress for the Glance application.
    ```bash
    kubectl apply -f Glance/glance.yaml
    ```

3.  **Access the dashboard:**
    Once the application is deployed, you can access it at `http://glance.init0xff.com` (or your own domain if you configured it). Note that you will need to have DNS configured to point this domain to the address of the ALB created by Traefik.

## 5. Deploy Uptime Kuma

Uptime Kuma is a popular open-source monitoring tool. The following steps will guide you through its deployment.

1.  **Create the `uptime` namespace:**
    ```bash
    kubectl create namespace uptime
    ```

2.  **Apply the Uptime Kuma manifest:**
    This will create the necessary resources, including a PersistentVolume, PersistentVolumeClaim, Deployment, Service, and Ingress.
    ```bash
    kubectl apply -f UptimeKuma/Uptime-Kuma.yaml
    ```

3.  **Access the Uptime Kuma dashboard:**
    Once deployed, you can access the Uptime Kuma dashboard at `http://uptime.init0xff.com` (or your configured domain). You will need to have DNS configured to point this domain to the address of the ALB.