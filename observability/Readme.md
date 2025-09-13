




helm install grafana grafana/grafana -n observability -f grafana/values.yaml
helm install mimir grafana/mimir-distributed -n observability -f mimir/values.yaml
helm install loki grafana/loki -n observability -f loki/values.yaml
helm install agent grafana/grafana-agent -n observability -f grafana-agent/values.yaml
helm install node-exporter prometheus-community/prometheus-node-exporter -n observability -f node-exporter/values.yaml
helm install kube-state-metrics prometheus-community/kube-state-metrics -n observability -f kube-state-metrics/values.yaml