eksctl create iamserviceaccount \
  --name mimir-sa \
  --namespace observability \
  --cluster home-lab-eks \
  --region ap-south-1 \
  --attach-policy-arn arn:aws:iam::058264195463:policy/mimir-s3-policy \
  --approve \
  --override-existing-serviceaccounts \
  --role-name home-lab-eks-mimir-s3-role