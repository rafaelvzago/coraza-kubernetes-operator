---
title: "Monitoring with Prometheus"
linkTitle: "Monitoring with Prometheus"
weight: 45
description: "Enable metrics collection and Prometheus monitoring for the operator."
---

The Coraza Kubernetes Operator exposes Prometheus metrics over HTTPS for monitoring the RuleSet cache server.

## Enabling the Metrics Endpoint

Metrics are enabled by default. The endpoint is served over HTTPS on port **8443** with TLS 1.3 and requires authentication via a Kubernetes ServiceAccount token.

To disable metrics:

```yaml
# values.yaml
metrics:
  enabled: false
```

## Enabling the ServiceMonitor

If you use the [Prometheus Operator](https://prometheus-operator.dev/), enable the ServiceMonitor to automatically discover the metrics endpoint:

```yaml
# values.yaml
metrics:
  serviceMonitor:
    enabled: true
```

## Configuring Prometheus RBAC

The metrics endpoint uses Kubernetes authentication. Prometheus must present a valid ServiceAccount token and the ServiceAccount must have permission to access the `/metrics` endpoint.

Create a ClusterRole and ClusterRoleBinding:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coraza-metrics-reader
rules:
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: coraza-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: coraza-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: monitoring
```

Adjust the ServiceAccount name and namespace to match your Prometheus installation.

## Using User-Provided TLS Certificates

By default, the operator generates a self-signed certificate for the metrics endpoint. To use your own certificate:

1. Create a Secret containing the TLS certificate and key:

   ```bash
   kubectl create secret tls metrics-tls \
     --cert=tls.crt --key=tls.key \
     -n coraza-system
   ```

2. Reference it in the Helm values:

   ```yaml
   metrics:
     certSecret: metrics-tls
     certName: tls.crt
     keyName: tls.key
     caName: ca.crt   # optional: for ServiceMonitor TLS verification
   ```

## Available Metrics

### RuleSet cache server (RED)

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_cache_server_requests_total` | Counter | Total number of requests. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_request_duration_seconds` | Histogram | Request duration in seconds. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_in_flight_requests` | Gauge | Number of in-flight requests. Labels: `handler`. |
| `coraza_cache_server_auth_failures_total` | Counter | Authentication failures on the cache HTTP server (invalid or missing bearer token). |

The `handler` label has two values:

- `rules` -- requests for the full compiled ruleset
- `latest` -- requests for the latest ruleset metadata

### Rule validation

Counters and histograms are emitted during Coraza validation in the RuleSource and RuleSet reconcilers. The `outcome` label is `valid`, `invalid`, or (RuleSource only) `skipped`. A `valid` outcome means Coraza parsing succeeded — it does not imply the resource is Ready.

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_rulesource_validations_total` | Counter | RuleSource validation outcomes. Labels: `namespace`, `outcome`. |
| `coraza_rulesource_validation_duration_seconds` | Histogram | RuleSource validation latency. Labels: `namespace`, `outcome` (`valid` or `invalid` only). |
| `coraza_ruleset_validations_total` | Counter | RuleSet aggregate validation outcomes. Labels: `namespace`, `outcome`. |
| `coraza_ruleset_validation_duration_seconds` | Histogram | RuleSet aggregate validation latency. Labels: `namespace`, `outcome`. |

### Cache storage

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_cache_set_duration_seconds` | Histogram | Time to store a compiled RuleSet in the in-memory cache. Labels: `namespace`. |

For controller resource gauges, condition metrics, and cardinality guidance, see [Metrics cardinality reference]({{< relref "../reference/metrics-cardinality" >}}).

When the Helm chart's `metrics.prometheusRule.enabled` value is true, bundled alerts cover validation failure rates, cache hit ratio, and authentication failures on the cache server. The same `PrometheusRule` also includes dataplane block-ratio alerts (see [Block ratio over time and spike alerts](#block-ratio-over-time-and-spike-alerts)).

## Data-plane WAF metrics (Gateway / WASM)

Control-plane metrics above are from the operator. Data-plane WAF observability uses the [`coraza_waf_*` contract](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/driver-metrics-contract.md).

### Operator-managed OpenTelemetry Collector sidecar

When the [OpenTelemetry Operator](https://opentelemetry.io/docs/kubernetes/operator/) CRD is installed on the cluster, the Engine reconciler **automatically** creates an `OpenTelemetryCollector` sidecar resource per Engine and patches the target Gateway's `spec.infrastructure.annotations` with `sidecar.opentelemetry.io/inject` so the OTel Operator injects the collector into Gateway pods.

No manual OTC YAML or Gateway annotation is required — the operator handles both.

If the OTel Operator CRD is absent, the operator skips OTC creation silently and WAF provisioning proceeds without data-plane metrics.

#### Scrape target

The OTC sidecar exposes a Prometheus endpoint on port **9090** inside the Gateway pod. Point a `PodMonitor` at pods matching the Gateway label:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: coraza-waf-dataplane
spec:
  selector:
    matchLabels:
      gateway.networking.k8s.io/gateway-name: <gateway-name>
  podMetricsEndpoints:
    - port: "9090"
      path: /metrics
```

#### Label enrichment

The OTC `transform/tenancy` processor stamps three labels on every log record before the `count` connector materializes metrics:

| Label | Value source |
|-------|--------------|
| `engine` | `Engine` CR `.metadata.name` |
| `namespace` | `Engine` CR `.metadata.namespace` |
| `gateway` | `Engine` CR `.spec.target.name` |

These labels appear as dimensions on all `coraza_waf_*` counter metrics below.

#### Implemented metrics

The OTC sidecar's `filelog/coraza` receiver reads all `coraza_waf_*` JSON log events from Gateway container logs and routes them to per-metric counters via `count` connector conditions:

| Metric | Log event condition | Extra labels |
|--------|-------------------|--------------|
| `coraza_waf_requests_total` | `event == "coraza_waf_request"` | `outcome` |
| `coraza_waf_blocked_requests_total` | `event == "coraza_waf_blocked_request"` | `category`, `severity` |
| `coraza_waf_plugin_loads_total` | `event == "coraza_waf_plugin_load"` | `status` |
| `coraza_waf_rule_hits_from_logs_total` | `event == "coraza_waf_blocked_request"` | `rule_id`, `severity`, `category` |

All four metrics carry the `engine`, `namespace`, and `gateway` tenancy labels.

> **Note:** `coraza_waf_requests_total` and `coraza_waf_blocked_requests_total` require the WASM plugin to emit structured JSON events (`coraza_waf_request`, `coraza_waf_blocked_request`). `coraza_waf_rule_hits_from_logs_total` is derived from the same `coraza_waf_blocked_request` events (blocking-rule hits only, using `rule_id` / `severity` / `category` already on those logs). Full per-match `coraza_waf_rule_hit` coverage for the contract metric `coraza_waf_rule_hits_total` remains a WASM follow-up. The filelog receiver uses `start_at: beginning` so initial `coraza_waf_plugin_load` lines already in the CRI log are not missed when the sidecar starts. The CRI regex accepts both `wasm log: {...}` (common for plugin-load before the filter name is set) and `wasm log <name>: {...}` (per-request events).

#### Pipelines

The OTC runs three internal pipelines:

| Pipeline | Receivers | Processors | Exporters |
|----------|-----------|------------|-----------|
| `metrics/envoy` | `prometheus/envoy` (scrapes `127.0.0.1:15090`) | — | `prometheus` (`:9090`) |
| `logs/coraza` | `filelog/coraza` | `transform/tenancy` | `count/coraza` |
| `metrics/coraza` | `count/coraza` | — | `prometheus` (`:9090`) |

The count connector emits delta-temporality sums; the Prometheus exporter accumulates these into cumulative counters internally, so no extra processor is needed. This also keeps the pipeline portable to the Red Hat build of OpenTelemetry, whose curated collector image doesn't ship the contrib-only `deltatocumulative` processor.

#### OpenShift

The `hostPath /var/log/pods` mount is blocked by the default `restricted` SCC. Create a custom SCC and bind it to the sidecar’s service account before deploying:

```yaml
apiVersion: security.openshift.io/v1
kind: SecurityContextConstraints
metadata:
  name: coraza-otel-sidecar-scc
allowPrivilegedContainer: false
requiredDropCapabilities:
  - ALL
allowHostDirVolumePlugin: true
volumes:
  - configMap
  - emptyDir
  - hostPath
  - projected
  - secret
defaultAllowPrivilegeEscalation: false
allowPrivilegeEscalation: false
runAsUser:
  type: RunAsAny
seLinuxContext:
  type: RunAsAny
readOnlyRootFilesystem: true
forbiddenSysctls:
  - "*"
seccompProfiles:
  - runtime/default
```

```bash
oc adm policy add-scc-to-user coraza-otel-sidecar-scc \
  -z <otel-sidecar-serviceaccount> -n <gateway-namespace>
```

`runAsUser` and `seLinuxContext` are set to `RunAsAny` so the collector can read SELinux-labelled log files without knowing the exact UID in advance. Scope them to `MustRunAsRange` once the sidecar’s actual UID is confirmed.

Additionally, JWT-protect the collector’s `:9090` exporter and scrape with a `PodMonitor` + `bearerTokenSecret`.

### Block ratio over time and spike alerts

**Block ratio** is blocked WAF-evaluated requests over all WAF-evaluated requests:

```promql
sum(rate(coraza_waf_requests_total{outcome="block"}[5m]))
/
clamp_min(sum(rate(coraza_waf_requests_total[5m])), 1e-9)
```

Use a short `rate` window (5m–15m) as the series and let the Grafana time picker (`now-24h`, `now-7d`) stretch the X axis. Do not hardcode a multi-day range inside the ratio — that collapses history into one number.

The sample Grafana dashboard (`config/observability/coraza-waf-gist-dashboard-configmap.yaml`) keeps **Block ratio (1h)** as a near-term pulse and adds **Block ratio over time** for weekend-style review. Retention is whatever Thanos/UWM keeps (often 24h–15d on OpenShift); confirm before promising 7d.

When `metrics.prometheusRule.enabled` is true, the chart also ships a `coraza-waf.dataplane.alerts` group (disable with `metrics.prometheusRule.dataplane.enabled=false`):

| Alert | Intent | Helm knobs |
|-------|--------|------------|
| `CorazaWAFBlockRatioHigh` | 15m ratio above threshold for 10m, with a traffic floor | `dataplane.blockRatioThreshold` (default `0.1`), `dataplane.minRequestRate` (default `0.1` req/s) |
| `CorazaWAFBlockRatioRising` | 15m ratio > 2h baseline × multiplier, same floor | `dataplane.blockRatioRisingMultiplier` (default `3`) |

Raise thresholds for noisy demos (continuous traffic generators often keep the ratio high). For OpenShift UWM before the next operator chart upgrade, a demo mirror lives at `config/observability/coraza-waf-block-ratio-prometheusrule.yaml`.

If long-range plots look flat, check that `coraza_waf_*` counters keep accumulating through the OTC path before trusting multi-day SOC review.

### Transitional: scrape Envoy stats

Gateway Envoy exposes legacy `waf_filter_*` series on `/stats/prometheus` (port **15090** `http-envoy-prom`). The OTC sidecar config already scrapes this endpoint and filters for WAF-related metrics. This path does **not** replace the contract log→metrics design and should not depend on EnvoyFilter `stats_tags`.
