#!/usr/bin/env bash
set -euo pipefail

# ==== CONFIG ====
REGION="${REGION:-ap-south-1}"
ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
CLUSTER_NAME="${CLUSTER_NAME:-home-lab-eks}"
NAMESPACE="${NAMESPACE:-observability}"

# ServiceAccounts (must match Helm values)
LOKI_SA="${LOKI_SA:-loki-sa}"
MIMIR_SA="${MIMIR_SA:-mimir-sa}"

# Buckets (MUST be globally unique)
LOKI_BUCKET="${LOKI_BUCKET:-home-lab-eks-loki}"
MIMIR_BLOCKS_BUCKET="${MIMIR_BLOCKS_BUCKET:-home-lab-eks-mimir-blocks}"
MIMIR_ALERTS_BUCKET="${MIMIR_ALERTS_BUCKET:-home-lab-eks-mimir-alerts}"
MIMIR_RULER_BUCKET="${MIMIR_RULER_BUCKET:-home-lab-eks-mimir-ruler}"

# Update values.yaml in-place (true/false)
UPDATE_VALUES="${UPDATE_VALUES:-true}"

# Paths to your repo values
REPO_ROOT="${REPO_ROOT:-/Users/nixndme/Desktop/HOMELAB}"
LOKI_VALUES="$REPO_ROOT/observability/loki/values.yaml"
MIMIR_VALUES="$REPO_ROOT/observability/mimir/values.yaml"

# ==== PREREQS ====
command -v aws >/dev/null || { echo "aws CLI not found"; exit 1; }
command -v jq >/dev/null || { echo "jq not found"; exit 1; }

# ==== EKS OIDC PROVIDER ====
ISSUER_URL="$(aws eks describe-cluster --name "$CLUSTER_NAME" --region "$REGION" --query "cluster.identity.oidc.issuer" --output text)"
if [[ -z "$ISSUER_URL" || "$ISSUER_URL" == "None" ]]; then
  echo "EKS cluster $CLUSTER_NAME has no OIDC issuer enabled."
  echo "Enable: eksctl utils associate-iam-oidc-provider --cluster $CLUSTER_NAME --region $REGION --approve"
  exit 1
fi
OIDC_HOST="${ISSUER_URL#https://}"
OIDC_PROVIDER_ARN="$(aws iam list-open-id-connect-providers --query "OpenIDConnectProviderList[].Arn" --output text \
  | tr '\t' '\n' | while read -r arn; do
      url="$(aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$arn" --query 'Url' --output text)"
      [[ "$url" == "$OIDC_HOST" ]] && echo "$arn"
    done | head -n1)"
[[ -z "$OIDC_PROVIDER_ARN" ]] && { echo "OIDC provider not found for $OIDC_HOST"; exit 1; }
echo "Using OIDC: $OIDC_PROVIDER_ARN ($ISSUER_URL)"

# ==== S3 BUCKETS ====
create_bucket() {
  local b="$1"
  if aws s3api head-bucket --bucket "$b" 2>/dev/null; then
    echo "Bucket exists: $b"
  else
    echo "Creating bucket: $b"
    aws s3api create-bucket --bucket "$b" --region "$REGION" --create-bucket-configuration LocationConstraint="$REGION"
  fi
  aws s3api put-bucket-versioning --bucket "$b" --versioning-configuration Status=Enabled
  aws s3api put-bucket-encryption --bucket "$b" --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  aws s3api put-public-access-block --bucket "$b" --public-access-block-configuration '{
    "BlockPublicAcls": true, "IgnorePublicAcls": true, "BlockPublicPolicy": true, "RestrictPublicBuckets": true
  }'
}
create_bucket "$LOKI_BUCKET"
create_bucket "$MIMIR_BLOCKS_BUCKET"
create_bucket "$MIMIR_ALERTS_BUCKET"
create_bucket "$MIMIR_RULER_BUCKET"

# ==== IAM POLICIES ====
LOKI_POLICY_NAME="home-lab-loki-s3-policy"
MIMIR_POLICY_NAME="home-lab-mimir-s3-policy"

create_or_get_policy_arn() {
  local name="$1" doc="$2"
  local arn
  arn="$(aws iam list-policies --scope Local --query "Policies[?PolicyName=='$name'].Arn" --output text)"
  if [[ -z "$arn" ]]; then
    echo "Creating policy: $name"
    arn="$(aws iam create-policy --policy-name "$name" --policy-document "$doc" --query 'Policy.Arn' --output text)"
  else
    echo "Policy exists: $name"
  fi
  echo "$arn"
}

LOKI_POLICY_DOC="$(jq -nc --arg b "$LOKI_BUCKET" '{
  Version:"2012-10-17",
  Statement:[
    {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::\($b)"]},
    {"Effect":"Allow","Action":["s3:PutObject","s3:GetObject","s3:DeleteObject","s3:AbortMultipartUpload","s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::\($b)/*"]}
  ]
}')"

MIMIR_POLICY_DOC="$(jq -nc --arg b1 "$MIMIR_BLOCKS_BUCKET" --arg b2 "$MIMIR_ALERTS_BUCKET" --arg b3 "$MIMIR_RULER_BUCKET" '{
  Version:"2012-10-17",
  Statement:[
    {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::\($b1)","arn:aws:s3:::\($b2)","arn:aws:s3:::\($b3)"]},
    {"Effect":"Allow","Action":["s3:PutObject","s3:GetObject","s3:DeleteObject","s3:AbortMultipartUpload","s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::\($b1)/*","arn:aws:s3:::\($b2)/*","arn:aws:s3:::\($b3)/*"]}
  ]
}')"

LOKI_POLICY_ARN="$(create_or_get_policy_arn "$LOKI_POLICY_NAME" "$LOKI_POLICY_DOC")"
MIMIR_POLICY_ARN="$(create_or_get_policy_arn "$MIMIR_POLICY_NAME" "$MIMIR_POLICY_DOC")"

# ==== IAM ROLES FOR IRSA ====
create_role_with_trust() {
  local role_name="$1" sa="$2"
  local trust
  trust="$(jq -nc --arg provider "$OIDC_PROVIDER_ARN" --arg host "$OIDC_HOST" --arg ns "$NAMESPACE" --arg sa "$sa" '{
    Version:"2012-10-17",
    Statement:[{
      Effect:"Allow",
      Principal:{Federated:$provider},
      Action:"sts:AssumeRoleWithWebIdentity",
      Condition:{
        StringEquals:{
          "\($host):aud":"sts.amazonaws.com",
          "\($host):sub":"system:serviceaccount:\($ns):\($sa)"
        }
      }
    }]
  }')"
  if aws iam get-role --role-name "$role_name" >/dev/null 2>&1; then
    echo "Role exists: $role_name"
  else
    echo "Creating role: $role_name"
    aws iam create-role --role-name "$role_name" --assume-role-policy-document "$trust" >/dev/null
  fi
}

LOKI_ROLE_NAME="home-lab-eks-loki-s3-role"
MIMIR_ROLE_NAME="home-lab-eks-mimir-s3-role"

create_role_with_trust "$LOKI_ROLE_NAME"  "$LOKI_SA"
create_role_with_trust "$MIMIR_ROLE_NAME" "$MIMIR_SA"

aws iam attach-role-policy --role-name "$LOKI_ROLE_NAME"  --policy-arn "$LOKI_POLICY_ARN"  || true
aws iam attach-role-policy --role-name "$MIMIR_ROLE_NAME" --policy-arn "$MIMIR_POLICY_ARN" || true

LOKI_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${LOKI_ROLE_NAME}"
MIMIR_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${MIMIR_ROLE_NAME}"

echo "LOKI_ROLE_ARN=$LOKI_ROLE_ARN"
echo "MIMIR_ROLE_ARN=$MIMIR_ROLE_ARN"

# ==== UPDATE values.yaml (optional) ====
if [[ "$UPDATE_VALUES" == "true" ]]; then
  if [[ -f "$LOKI_VALUES" ]]; then
    sed -i '' -E "s|(bucket_name:\s*).*$|\1$LOKI_BUCKET|g" "$LOKI_VALUES"
    sed -i '' -E "s|(region:\s*).*$|\1$REGION|g" "$LOKI_VALUES"
    if grep -q "eks.amazonaws.com/role-arn:" "$LOKI_VALUES"; then
      sed -i '' -E "s|(eks\.amazonaws\.com/role-arn:\s*).*$|\1$LOKI_ROLE_ARN|g" "$LOKI_VALUES"
    fi
  fi

  if [[ -f "$MIMIR_VALUES" ]]; then
    # Replace each bucket explicitly
    awk -v blocks="$MIMIR_BLOCKS_BUCKET" -v alerts="$MIMIR_ALERTS_BUCKET" -v ruler="$MIMIR_RULER_BUCKET" -v region="$REGION" '
      BEGIN{in_blocks=0; in_alerts=0; in_ruler=0}
      /blocks_storage:/ {in_blocks=1; in_alerts=0; in_ruler=0}
      /alertmanager_storage:/ {in_blocks=0; in_alerts=1; in_ruler=0}
      /ruler_storage:/ {in_blocks=0; in_alerts=0; in_ruler=1}
      {
        if (in_blocks && $0 ~ /bucket_name:/) sub(/bucket_name:.*/, "bucket_name: " blocks)
        if (in_alerts && $0 ~ /bucket_name:/) sub(/bucket_name:.*/, "bucket_name: " alerts)
        if (in_ruler && $0 ~ /bucket_name:/) sub(/bucket_name:.*/, "bucket_name: " ruler)
        if ($0 ~ /^[[:space:]]*region:/) sub(/region:.*/, "region: " region)
        print
      }' "$MIMIR_VALUES" > "$MIMIR_VALUES.tmp" && mv "$MIMIR_VALUES.tmp" "$MIMIR_VALUES"

    if grep -q "eks.amazonaws.com/role-arn:" "$MIMIR_VALUES"; then
      sed -i '' -E "s|(eks\.amazonaws\.com/role-arn:\s*).*$|\1$MIMIR_ROLE_ARN|g" "$MIMIR_VALUES"
    fi
  fi

  echo "Updated values files:"
  echo " - $LOKI_VALUES"
  echo " - $MIMIR_VALUES"
else
  echo "Skipping values.yaml updates (UPDATE_VALUES=false)."
  echo "Use these ARNs in your charts:"
  echo "  Loki SA annotation:  eks.amazonaws.com/role-arn: $LOKI_ROLE_ARN"
  echo "  Mimir SA annotation: eks.amazonaws.com/role-arn: $MIMIR_ROLE_ARN"
fi

echo "Done."
