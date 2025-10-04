ADMIN_TOKEN=$(openssl rand -base64 48)
echo "Admin Token: $ADMIN_TOKEN"

# Create secret
kubectl create secret generic vaultwarden-secrets -n vaultwarden --from-literal=ADMIN_TOKEN="$ADMIN_TOKEN"

