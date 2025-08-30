# HOMELAB


aws iam create-policy --policy-name EKSCA --policy-document file://iam_policy.json
> eksctl create iamserviceaccount --cluster=home-lab-eks --namespace=kube-system --name=cluster-autoscaler --attach-policy-arn=arn:aws:iam::<   >:policy/EKSCA --override-existing-serviceaccounts --approve --region=ap-south-1