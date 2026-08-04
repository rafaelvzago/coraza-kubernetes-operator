/*
Copyright Coraza Kubernetes Operator contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - OTC RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=opentelemetry.io,resources=opentelemetrycollectors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=patch

// -----------------------------------------------------------------------------
// Engine Controller - OTC Constants
// -----------------------------------------------------------------------------

const (
	otcNamePrefix       = "coraza-otc-"
	otcInjectAnnotation = "sidecar.opentelemetry.io/inject"
	maxDNSSubdomainLen  = 253
)

var otcGroupKind = schema.GroupKind{Group: "opentelemetry.io", Kind: "OpenTelemetryCollector"}

// otcName returns the deterministic operator-owned name for the OTC resource.
// Names exceeding 253 chars are truncated with a stable FNV-1a hash suffix.
func otcName(engineName string) string {
	name := otcNamePrefix + engineName
	if len(name) <= maxDNSSubdomainLen {
		return name
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	suffix := fmt.Sprintf("-%08x", h.Sum32())
	return name[:maxDNSSubdomainLen-len(suffix)] + suffix
}

// -----------------------------------------------------------------------------
// Engine Controller - OTC Labels (#20)
// -----------------------------------------------------------------------------

// engineOTCLabels returns the label map for the operator-owned OTC resource.
// The map carries both Engine tenancy labels (engine-name, engine-namespace)
// and the Gateway API gateway-name label for collector sidecar selection.
func engineOTCLabels(engine *wafv1alpha1.Engine) map[string]string {
	return map[string]string{
		networkPolicyEngineLabelName:      engine.Name,
		networkPolicyEngineLabelNamespace: engine.Namespace,
		gatewayNameLabel:                  engine.Spec.Target.Name,
	}
}

// tenancyAttributes returns the base engine/namespace/gateway attribute slice
// for count connector metric definitions, with extra attributes appended.
func tenancyAttributes(extra ...map[string]any) []any {
	attrs := make([]any, 0, 3+len(extra))
	attrs = append(attrs,
		map[string]any{"key": "engine", "default_value": ""},
		map[string]any{"key": "namespace", "default_value": ""},
		map[string]any{"key": "gateway", "default_value": ""},
	)
	for _, e := range extra {
		attrs = append(attrs, e)
	}
	return attrs
}

// -----------------------------------------------------------------------------
// Engine Controller - OTC CRD Detection
// -----------------------------------------------------------------------------

// otcCRDAvailable returns true when the OpenTelemetryCollector CRD is registered
// in the API server's REST mapper.
func otcCRDAvailable(c client.Client) bool {
	_, err := c.RESTMapper().RESTMapping(otcGroupKind)
	return err == nil
}

// -----------------------------------------------------------------------------
// Engine Controller - OTC Builder (#21)
// -----------------------------------------------------------------------------

// buildOTC constructs the operator-owned OpenTelemetryCollector resource for the
// given Engine and Gateway. The config runs two pipelines:
//   - metrics/envoy: Prometheus scrape of Envoy stats (127.0.0.1:15090)
//   - logs/coraza → metrics/coraza: filelog reader parsing all coraza_waf_*
//     JSON events, routed to per-metric counters via count connector conditions
func buildOTC(engine *wafv1alpha1.Engine) *unstructured.Unstructured {
	gatewayName := engine.Spec.Target.Name
	// Glob matches istio-proxy container logs for the target Gateway on the host.
	// Pattern: /var/log/pods/<namespace>_<gateway>-*_*/istio-proxy/*.log
	logGlob := fmt.Sprintf("/var/log/pods/%s_%s-*_*/istio-proxy/*.log", engine.Namespace, gatewayName)

	otc := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "opentelemetry.io/v1beta1",
			"kind":       "OpenTelemetryCollector",
			"metadata": map[string]any{
				"name":      otcName(engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"mode": "sidecar",
				// hostPath log files under /var/log/pods are owned root:root, mode
				// 0640 (kubelet/CRI-O convention), and the directory itself carries
				// the container_log_t SELinux type. Without both fixes below the
				// filelog/coraza receiver gets permission denied and never reads a
				// single Coraza log line:
				//   - runAsGroup:0 makes root's group the container's primary GID,
				//     satisfying the file's group-read bit via plain DAC — no
				//     elevated capabilities or root uid needed.
				//   - seLinuxOptions.type:spc_t (the standard "super-privileged
				//     container" type used by hostPath log-collector sidecars,
				//     e.g. OpenShift Logging's collector) is required in addition:
				//     confirmed via node-level `ausearch -m avc` that plain
				//     container_t is denied `read` on the container_log_t
				//     /var/log/pods directory even once DAC group-read is
				//     satisfied. spc_t only relaxes SELinux (MAC); it does not
				//     bypass the DAC check above, so both settings are required.
				"securityContext": map[string]any{
					"runAsGroup": int64(0),
					"seLinuxOptions": map[string]any{
						"type": "spc_t",
					},
				},
				// hostPath volume so the sidecar can read containerd log files.
				"volumes": []any{
					map[string]any{
						"name":     "varlogpods",
						"hostPath": map[string]any{"path": "/var/log/pods"},
					},
				},
				"volumeMounts": []any{
					map[string]any{
						"name":      "varlogpods",
						"mountPath": "/var/log/pods",
						"readOnly":  true,
					},
				},
				"config": map[string]any{
					"receivers": map[string]any{
						"prometheus/envoy": map[string]any{
							"config": map[string]any{
								"scrape_configs": []any{
									map[string]any{
										"job_name":        "coraza-envoy",
										"metrics_path":    "/stats/prometheus",
										"scrape_interval": "15s",
										"static_configs": []any{
											map[string]any{"targets": []any{"127.0.0.1:15090"}},
										},
									},
								},
							},
						},
						"filelog/coraza": map[string]any{
							"include": []any{logGlob},
							// Catch plugin_load (and other) events already written to the
							// current CRI log file before the sidecar started. Without this
							// the default start_at=end races past the initial load line.
							"start_at": "beginning",
							"operators": []any{
								// Strip CRI prefix and filter to Coraza WAF JSON lines in one pass.
								// Lines that don't match (text logs, health checks, other wasm plugins) are dropped.
								// The "body" capture group is written to attributes["body"] by default.
								map[string]any{
									"type": "regex_parser",
									// Plugin-load lines often look like "wasm log: {...}" (no filter
									// name) while per-request lines look like
									// "wasm log <ns>.<name>: {...}". Make the name segment optional.
									"regex":    `^\S+ \S+ F .*wasm log(?: [^:]+)?: (?P<body>\{.*"event":"coraza_waf_[^"]*".*\})`,
									"on_error": "drop",
								},
								// Parse the extracted JSON string into individual log attributes.
								map[string]any{
									"type":       "json_parser",
									"parse_from": "attributes.body",
									"parse_to":   "attributes",
									"on_error":   "drop",
								},
							},
						},
					},
					"processors": map[string]any{
						// No delta→cumulative processor here: the count connector emits
						// delta-temporality sums, and the core prometheus exporter already
						// accumulates delta sums into cumulative counters internally
						// (see contrib exporter/prometheusexporter/accumulator.go), so no
						// extra processor is needed. This also keeps the pipeline portable
						// to the Red Hat build of OpenTelemetry, whose curated collector
						// image does not ship the contrib-only "deltatocumulative" processor.
						// Stamp engine/namespace/gateway as log attributes so the count
						// connector can use them as metric label dimensions. Values are
						// known at OTC creation time from the Engine CR.
						"transform/tenancy": map[string]any{
							"log_statements": []any{
								map[string]any{
									"context": "log",
									"statements": []any{
										fmt.Sprintf(`set(attributes["engine"], "%s")`, engine.Name),
										fmt.Sprintf(`set(attributes["namespace"], "%s")`, engine.Namespace),
										fmt.Sprintf(`set(attributes["gateway"], "%s")`, gatewayName),
										// Count-connector metric labels need strings; blocked_request
										// logs emit rule_id as a JSON number.
										`set(attributes["rule_id"], Concat([attributes["rule_id"]], "")) where attributes["rule_id"] != nil`,
									},
								},
							},
						},
					},
					"connectors": map[string]any{
						"count/coraza": map[string]any{
							"logs": map[string]any{
								"coraza_waf_requests_total": map[string]any{
									"description": "Total WAF requests by outcome",
									"conditions":  []any{`attributes["event"] == "coraza_waf_request"`},
									"attributes": tenancyAttributes(
										map[string]any{"key": "outcome", "default_value": "unknown"},
									),
								},
								"coraza_waf_blocked_requests_total": map[string]any{
									"description": "Total blocked WAF requests by category and severity",
									"conditions":  []any{`attributes["event"] == "coraza_waf_blocked_request"`},
									"attributes": tenancyAttributes(
										map[string]any{"key": "category", "default_value": "unknown"},
										map[string]any{"key": "severity", "default_value": "unknown"},
									),
								},
								"coraza_waf_plugin_loads_total": map[string]any{
									"description": "Total WAF plugin loads by status",
									"conditions":  []any{`attributes["event"] == "coraza_waf_plugin_load"`},
									"attributes": tenancyAttributes(
										map[string]any{"key": "status", "default_value": "unknown"},
									),
								},
								// Blocking-rule hits only: reuse coraza_waf_blocked_request which
								// already carries rule_id/severity/category. Full per-match
								// coraza_waf_rule_hit (detect/pass + top-N) remains a WASM follow-up.
								"coraza_waf_rule_hits_from_logs_total": map[string]any{
									"description": "Total WAF blocking rule hits by rule, severity, and category",
									"conditions":  []any{`attributes["event"] == "coraza_waf_blocked_request"`},
									"attributes": tenancyAttributes(
										map[string]any{"key": "rule_id", "default_value": "unknown"},
										map[string]any{"key": "severity", "default_value": "unknown"},
										map[string]any{"key": "category", "default_value": "unknown"},
									),
								},
							},
						},
					},
					"exporters": map[string]any{
						"prometheus": map[string]any{
							"endpoint": "0.0.0.0:9090",
						},
					},
					"service": map[string]any{
						"pipelines": map[string]any{
							"metrics/envoy": map[string]any{
								"receivers": []any{"prometheus/envoy"},
								"exporters": []any{"prometheus"},
							},
							"logs/coraza": map[string]any{
								"receivers":  []any{"filelog/coraza"},
								"processors": []any{"transform/tenancy"},
								"exporters":  []any{"count/coraza"},
							},
							// The prometheus exporter accumulates the count connector's
							// delta-temporality sums into cumulative counters itself, so
							// this pipeline needs no delta→cumulative processor.
							"metrics/coraza": map[string]any{
								"receivers": []any{"count/coraza"},
								"exporters": []any{"prometheus"},
							},
						},
					},
				},
			},
		},
	}
	otc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "opentelemetry.io",
		Version: "v1beta1",
		Kind:    "OpenTelemetryCollector",
	})
	otc.SetLabels(engineOTCLabels(engine))
	return otc
}

// -----------------------------------------------------------------------------
// Engine Controller - OTC Reconcile (#21)
// -----------------------------------------------------------------------------

// reconcileOTC creates/updates the operator-owned OTC for the Engine's target
// Gateway and stamps the sidecar inject annotation on the Gateway's infrastructure
// annotations. When otcAvailable is false (CRD absent), returns nil immediately
// so WAF provisioning is never blocked by optional observability.
func (r *EngineReconciler) reconcileOTC(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, otcAvailable bool) error {
	if !otcAvailable {
		logDebug(log, req, "Engine", "OpenTelemetryCollector CRD absent, skipping OTC reconcile")
		return nil
	}

	gatewayName := engine.Spec.Target.Name
	if gatewayName == "" {
		return nil
	}

	otc := buildOTC(engine)
	if err := controllerutil.SetControllerReference(engine, otc, r.Scheme); err != nil {
		return fmt.Errorf("set OTC owner reference: %w", err)
	}

	logDebug(log, req, "Engine", "Applying OTC", "otcName", otc.GetName())
	if err := serverSideApply(ctx, r.Client, otc); err != nil {
		return fmt.Errorf("apply OTC: %w", err)
	}

	logDebug(log, req, "Engine", "Patching Gateway inject annotation", "gateway", gatewayName)
	if err := r.patchGatewayInjectAnnotation(ctx, engine, otcName(engine.Name)); err != nil {
		return fmt.Errorf("patch gateway inject annotation: %w", err)
	}

	logInfo(log, req, "Engine", "OTC reconciled", "otcName", otc.GetName(), "gateway", gatewayName)
	return nil
}

// patchGatewayInjectAnnotation sets sidecar.opentelemetry.io/inject on the
// Gateway's spec.infrastructure.annotations so the OTel Operator injects the
// OTC sidecar into Gateway pods.
func (r *EngineReconciler) patchGatewayInjectAnnotation(ctx context.Context, engine *wafv1alpha1.Engine, injectName string) error {
	patch := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]any{
				"name":      engine.Spec.Target.Name,
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"infrastructure": map[string]any{
					"annotations": map[string]any{
						otcInjectAnnotation: injectName,
					},
				},
			},
		},
	}
	patch.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})
	if err := r.Client.Patch(ctx, patch, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("patch gateway %s/%s: %w", engine.Namespace, engine.Spec.Target.Name, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Engine Controller - OTC Integration in provisionWasmDriver
// -----------------------------------------------------------------------------

// reconcileOTCBestEffort runs reconcileOTC and logs any error without
// propagating it so WAF provisioning is never blocked by optional observability.
func (r *EngineReconciler) reconcileOTCBestEffort(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) {
	available := otcCRDAvailable(r.Client)
	if err := r.reconcileOTC(ctx, log, req, engine, available); err != nil {
		logError(log, req, "Engine", err, "OTC reconcile failed (non-blocking)")
		r.Recorder.Eventf(engine, nil, "Warning", "OTCFailed", "OTC", "OTC reconcile failed (non-blocking): %v", err)
	}
}
