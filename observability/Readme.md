# Observability Stack (single namespace, single-binary Loki & Mimir)

This installs Grafana, Loki (single-binary), Mimir (monolithic/single-binary via mimir-distributed), Grafana Agent (Flow), Node Exporter, and Kube State Metrics into the `observability` namespace. Grafana is exposed via Traefik at `grafana.init0xff.com`. ALB terminates TLS and forwards HTTP to Traefik `web` entrypoint.

## Prerequisites
- AWS CLI + jq installed and configured.
- EKS cluster reachable; Traefik and ALB wildcard `*.init0xff.com` already routing to Traefik `web`.
- Helm repos:
  - grafana: https://grafana.github.io/helm-charts
  - prometheus-community: https://prometheus-community.github.io/helm-charts

## 1) Create namespace
kubectl create ns observability || true

## 2) Provision S3 + IRSA
chmod +x /Users/nixndme/Desktop/HOMELAB/observability/provision-observability-aws.sh
/Users/nixndme/Desktop/HOMELAB/observability/provision-observability-aws.sh

This:
- Creates S3 buckets in ap-south-1: Loki, Mimir blocks/alerts/ruler (versioning, SSE, block public).
- Creates IRSA roles for `loki-sa` and `mimir-sa` in `observability`.
- Updates `observability/loki/values.yaml` and `observability/mimir/values.yaml` with bucket names, region, and role ARNs.

## 3) Add Helm repos (once)
helm repo add grafana https://grafana.github.io/helm-charts
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

## 4) Install components (single namespace)
# Mimir (monolithic) with gateway (nginx)
helm upgrade --install mimir grafana/mimir-distributed -n observability -f mimir/values.yaml

# Loki (single-binary)
helm upgrade --install loki grafana/loki -n observability -f loki/values.yaml

# Grafana Agent (Flow) – sends metrics to Mimir (via mimir-nginx) and logs to Loki
helm upgrade --install agent grafana/grafana-agent -n observability -f grafana-agent/values.yaml

# Node Exporter and Kube State Metrics
helm upgrade --install node-exporter prometheus-community/prometheus-node-exporter -n observability -f node-exporter/values.yaml
helm upgrade --install kube-state-metrics prometheus-community/kube-state-metrics -n observability -f kube-state-metrics/values.yaml

# Grafana (with Traefik ingress grafana.init0xff.com)
helm upgrade --install grafana grafana/grafana -n observability -f grafana/values.yaml

## 5) Verify single-binary (expect only these pods)
kubectl get pods -n observability
# Expected:
# - 1x loki single-binary pod
# - 1x mimir single-binary pod (+ 1x mimir-nginx)
# - grafana, grafana-agent, kube-state-metrics, node-exporter (DaemonSet)

If you see extra pods like compactor/ingester/querier, re-check:
- Loki: deploymentMode=SingleBinary; all other components disabled.
- Mimir: mimir.mode=monolithic; only singleBinary + gateway enabled; all others disabled.

## 6) Access Grafana
Open https://grafana.init0xff.com
- Traefik Ingress uses entrypoint `web` (no TLS at Traefik).
- Admin password is set in `observability/grafana/values.yaml` (`adminPassword`).

## Notes
- Grafana datasources:
  - Mimir: http://mimir-nginx.observability.svc.cluster.local/prometheus
  - Loki:  http://loki.observability.svc.cluster.local
- Grafana Agent remote_write/logs:
  - Mimir push: http://mimir-nginx.observability.svc.cluster.local/api/v1/push
  - Loki push:  http://loki.observability.svc.cluster.local/loki/api/v1/push