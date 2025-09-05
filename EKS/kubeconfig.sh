#!/bin/bash

# Script to create a new admin Service Account and generate static kubeconfig for EKS cluster

# Usage: ./create-admin-kubeconfig.sh <output_file> (default: ~/static-kubeconfig.yaml)

OUTPUT_FILE="${1:-$HOME/static-kubeconfig.yaml}"
SA_NAME="aswath-sa"
SECRET_NAME="aswath-sa-token"
NAMESPACE="kube-system"
BINDING_NAME="${SA_NAME}-binding"

# Detect OS for base64 decode
if [[ "$OSTYPE" == "darwin"* ]]; then
  BASE64_DECODE="base64 -d"
else
  BASE64_DECODE="base64 --decode"
fi

# Step 1: Create Service Account if not exists
if ! kubectl get serviceaccount $SA_NAME -n $NAMESPACE > /dev/null 2>&1; then
  kubectl create serviceaccount $SA_NAME -n $NAMESPACE || { echo "Failed to create SA"; exit 1; }
else
  echo "ServiceAccount $SA_NAME already exists, skipping creation."
fi

# Step 2: Create ClusterRoleBinding if not exists
if ! kubectl get clusterrolebinding $BINDING_NAME > /dev/null 2>&1; then
  kubectl create clusterrolebinding $BINDING_NAME --clusterrole=cluster-admin --serviceaccount=${NAMESPACE}:${SA_NAME} || { echo "Failed to create binding"; exit 1; }
else
  echo "ClusterRoleBinding $BINDING_NAME already exists, skipping creation."
fi

# Step 3: Create Secret for token if not exists (using apply for idempotency)
if ! kubectl get secret $SECRET_NAME -n $NAMESPACE > /dev/null 2>&1; then
  kubectl apply -f - <<EOF || { echo "Failed to create Secret"; exit 1; }
apiVersion: v1
kind: Secret
metadata:
  name: $SECRET_NAME
  namespace: $NAMESPACE
  annotations:
    kubernetes.io/service-account.name: $SA_NAME
type: kubernetes.io/service-account-token
EOF
else
  echo "Secret $SECRET_NAME already exists, skipping creation."
fi

# Wait for Secret to be ready (token populated)
sleep 5  # Simple delay to ensure token is generated

# Step 4: Extract token
TOKEN=$(kubectl get secret $SECRET_NAME -n $NAMESPACE -o jsonpath='{.data.token}' | $BASE64_DECODE) || { echo "Failed to get token"; exit 1; }

# Step 5: Get cluster details from current kubeconfig
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}') || { echo "Failed to get server"; exit 1; }
CA_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}') || { echo "Failed to get CA data"; exit 1; }
CLUSTER_NAME=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}') || { echo "Failed to get cluster name"; exit 1; }

# Step 6: Generate kubeconfig
cat <<EOF > "$OUTPUT_FILE"
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: $CA_DATA
    server: $SERVER
  name: $CLUSTER_NAME
contexts:
- context:
    cluster: $CLUSTER_NAME
    user: $SA_NAME
  name: ${CLUSTER_NAME}-static
current-context: ${CLUSTER_NAME}-static
preferences: {}
users:
- name: $SA_NAME
  user:
    token: $TOKEN
EOF

echo "Static kubeconfig created at $OUTPUT_FILE"
echo "To use it: export KUBECONFIG=$OUTPUT_FILE"
echo "Test with: kubectl get po -A"