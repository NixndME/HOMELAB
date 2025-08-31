# HOMELAB

aws eks update-kubeconfig --name home-lab-eks --region ap-south-1

helm repo add eks https://aws.github.io/eks-charts
helm repo update

helm install aws-load-balancer-controller eks/aws-load-balancer-controller \
  -n kube-system \
  --set clusterName=home-lab-eks \
  --set serviceAccount.create=true \
  --set serviceAccount.name=aws-load-balancer-controller


kubectl get deploy/aws-load-balancer-controller -n kube-system
kubectl logs deploy/aws-load-balancer-controller -n kube-system  # Check for errors


helm repo add traefik https://traefik.github.io/charts
helm repo update

helm install traefik traefik/traefik -n kube-system --create-namespace -f traefik-values.yaml

kubectl get pods -n kube-system -l app.kubernetes.io/name=traefik
kubectl get svc -n kube-system traefik  # Should be ClusterIP with ports like web:8000
kubectl get ingressclass traefik  # Confirm it's default
kubectl logs deploy/traefik -n kube-system  # Check startup logs

kubectl apply -f traefik-alb-ingress.yaml

kubectl get ingress/traefik-alb -n kube-system -w  # Watch until .status.loadBalancer.ingress.hostname populates (e.g., k8s-kubesyst-traefika-abc123.elb.ap-south-1.amazonaws.com)
kubectl describe ingress/traefik-alb -n kube-system  # Check events for issues

