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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// -----------------------------------------------------------------------------
// #20 — engineOTCLabels
// -----------------------------------------------------------------------------

func TestEngineOTCLabels(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "my-engine",
		Namespace:   "prod",
		GatewayName: "my-gateway",
	})

	labels := engineOTCLabels(engine)

	assert.Equal(t, "my-engine", labels[networkPolicyEngineLabelName], "engine-name label")
	assert.Equal(t, "prod", labels[networkPolicyEngineLabelNamespace], "engine-namespace label")
	assert.Equal(t, "my-gateway", labels[gatewayNameLabel], "gateway-name label")
}

func TestBuildOTC_Labels(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "eng",
		Namespace:   "ns",
		GatewayName: "gw",
	})

	otc := buildOTC(engine)

	assert.Equal(t, "opentelemetry.io/v1beta1", otc.GetAPIVersion())
	assert.Equal(t, "OpenTelemetryCollector", otc.GetKind())
	assert.Equal(t, otcName("eng"), otc.GetName())
	assert.Equal(t, "ns", otc.GetNamespace())

	lbls := otc.GetLabels()
	assert.Equal(t, "eng", lbls[networkPolicyEngineLabelName])
	assert.Equal(t, "ns", lbls[networkPolicyEngineLabelNamespace])
	assert.Equal(t, "gw", lbls[gatewayNameLabel])
}

func TestOTCName(t *testing.T) {
	assert.Equal(t, "coraza-otc-my-engine", otcName("my-engine"))

	// Long name must be capped at 253 chars with a stable hash suffix.
	longEngine := strings.Repeat("a", 300)
	got := otcName(longEngine)
	assert.LessOrEqual(t, len(got), 253)
	assert.Equal(t, got, otcName(longEngine), "hash suffix must be stable")
}

func TestBuildOTC_Pipeline(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "eng",
		Namespace:   "ns",
		GatewayName: "gw",
	})
	otc := buildOTC(engine)

	spec, ok := otc.Object["spec"].(map[string]any)
	require.True(t, ok, "spec must be a map")

	cfg, ok := spec["config"].(map[string]any)
	require.True(t, ok, "spec.config must be a map")

	// Receivers: both envoy scrape and filelog must be present.
	receivers, _ := cfg["receivers"].(map[string]any)
	assert.Contains(t, receivers, "prometheus/envoy", "Envoy scrape receiver")
	assert.Contains(t, receivers, "filelog/coraza", "Coraza filelog receiver")

	// filelog glob embeds namespace and gateway name.
	fl, _ := receivers["filelog/coraza"].(map[string]any)
	includes, _ := fl["include"].([]any)
	require.Len(t, includes, 1)
	assert.Contains(t, includes[0], "ns_gw-", "log glob contains namespace_gateway prefix")
	assert.Equal(t, "beginning", fl["start_at"], "filelog must start_at beginning to catch initial plugin_load")

	// Operators: exactly 2 (combined CRI+filter regex, then json_parser reading attributes.body).
	operators, _ := fl["operators"].([]any)
	require.Len(t, operators, 2, "filelog must have exactly 2 operators")
	op0, _ := operators[0].(map[string]any)
	assert.Equal(t, "regex_parser", op0["type"])
	assert.Contains(t, op0["regex"], `"event":"coraza_waf_`, "regex must filter Coraza JSON events")
	assert.Contains(t, op0["regex"], `wasm log(?: [^:]+)?:`,
		"regex must accept both 'wasm log:' and 'wasm log <name>:' prefixes")
	op1, _ := operators[1].(map[string]any)
	assert.Equal(t, "json_parser", op1["type"])
	assert.Equal(t, "attributes.body", op1["parse_from"], "json_parser must read from attributes.body")

	// Connectors: count/coraza must have all 4 metrics with conditions.
	connectors, _ := cfg["connectors"].(map[string]any)
	assert.Contains(t, connectors, "count/coraza", "count connector")
	cc, _ := connectors["count/coraza"].(map[string]any)
	ccLogs, _ := cc["logs"].(map[string]any)

	expectedMetrics := []string{
		"coraza_waf_requests_total",
		"coraza_waf_blocked_requests_total",
		"coraza_waf_plugin_loads_total",
		"coraza_waf_rule_hits_from_logs_total",
	}
	for _, name := range expectedMetrics {
		assert.Contains(t, ccLogs, name, "count connector must define %s", name)
		m, _ := ccLogs[name].(map[string]any)
		assert.Contains(t, m, "conditions", "%s must have conditions", name)
		// Every metric must carry the tenancy labels.
		attrs, _ := m["attributes"].([]any)
		var keys []string
		for _, a := range attrs {
			if am, ok := a.(map[string]any); ok {
				keys = append(keys, am["key"].(string))
			}
		}
		assert.Contains(t, keys, "engine", "%s must have engine attr", name)
		assert.Contains(t, keys, "namespace", "%s must have namespace attr", name)
		assert.Contains(t, keys, "gateway", "%s must have gateway attr", name)
		assert.NotContains(t, keys, "engine_namespace", "%s must NOT use engine_namespace", name)
	}

	ruleHits, _ := ccLogs["coraza_waf_rule_hits_from_logs_total"].(map[string]any)
	conds, _ := ruleHits["conditions"].([]any)
	require.Len(t, conds, 1)
	assert.Equal(t, `attributes["event"] == "coraza_waf_blocked_request"`, conds[0],
		"rule hits interim metric must count blocked_request until WASM emits rule_hit")

	// Processors: only transform/tenancy — no deltatocumulative. That processor
	// is contrib-only and unsupported by the Red Hat build of OpenTelemetry;
	// the prometheus exporter accumulates delta sums into cumulative counters
	// internally, so it isn't needed.
	processors, _ := cfg["processors"].(map[string]any)
	assert.NotContains(t, processors, "deltatocumulative", "deltatocumulative is unsupported by the Red Hat OTel distro")
	tt, _ := processors["transform/tenancy"].(map[string]any)
	ttStmts, _ := tt["log_statements"].([]any)
	require.NotEmpty(t, ttStmts, "transform/tenancy must have log_statements")
	ttCtx, _ := ttStmts[0].(map[string]any)
	ttLines, _ := ttCtx["statements"].([]any)
	var tenancyStmtStr string
	for _, s := range ttLines {
		tenancyStmtStr += s.(string) + " "
	}
	assert.Contains(t, tenancyStmtStr, `attributes["namespace"]`, "transform/tenancy must stamp 'namespace'")
	assert.NotContains(t, tenancyStmtStr, `attributes["engine_namespace"]`, "transform/tenancy must NOT stamp 'engine_namespace'")

	// Pipelines: three pipelines wired correctly.
	svc, _ := cfg["service"].(map[string]any)
	pipelines, _ := svc["pipelines"].(map[string]any)
	assert.Contains(t, pipelines, "metrics/envoy")
	assert.Contains(t, pipelines, "logs/coraza")
	assert.Contains(t, pipelines, "metrics/coraza")

	// metrics/envoy pipeline must be wired to prometheus/envoy receiver → prometheus exporter.
	me, _ := pipelines["metrics/envoy"].(map[string]any)
	meRecv, _ := me["receivers"].([]any)
	meExp, _ := me["exporters"].([]any)
	assert.Contains(t, meRecv, "prometheus/envoy", "metrics/envoy receivers")
	assert.Contains(t, meExp, "prometheus", "metrics/envoy exporters")

	// metrics/coraza pipeline must be wired directly from count connector to
	// prometheus exporter, with no unsupported processor in between.
	mc, _ := pipelines["metrics/coraza"].(map[string]any)
	mcRecv, _ := mc["receivers"].([]any)
	mcExp, _ := mc["exporters"].([]any)
	assert.Contains(t, mcRecv, "count/coraza", "metrics/coraza receivers")
	assert.Contains(t, mcExp, "prometheus", "metrics/coraza exporters")
	assert.NotContains(t, mc, "processors", "metrics/coraza must not reference any processor")

	// volumes/volumeMounts must be present for hostPath access.
	assert.Contains(t, spec, "volumes", "hostPath volume")
	assert.Contains(t, spec, "volumeMounts", "volumeMount")

	// securityContext.runAsGroup must be 0 (root's group) so the sidecar's
	// filelog/coraza receiver can read root:root-owned, mode-0640 hostPath log
	// files via DAC group-read, without granting extra capabilities or root uid.
	secCtx, ok := spec["securityContext"].(map[string]any)
	require.True(t, ok, "spec.securityContext must be a map")
	assert.Equal(t, int64(0), secCtx["runAsGroup"], "otc-container must run with gid 0 to read hostPath Coraza logs")

	// seLinuxOptions.type must be spc_t: confirmed via node-level AVC denials
	// that plain container_t is denied read on the container_log_t /var/log/pods
	// directory even with DAC group-read satisfied.
	seLinux, ok := secCtx["seLinuxOptions"].(map[string]any)
	require.True(t, ok, "spec.securityContext.seLinuxOptions must be a map")
	assert.Equal(t, "spc_t", seLinux["type"], "otc-container must run as spc_t to read hostPath Coraza logs under SELinux")
}

// -----------------------------------------------------------------------------
// #21 — reconcileOTC CRD-absent path
// -----------------------------------------------------------------------------

func TestReconcileOTC_CRDAbsent(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "eng",
		Namespace:   "ns",
		GatewayName: "gw",
	})

	r := &EngineReconciler{
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		operatorNamespace: testNamespace,
	}

	logger := log.FromContext(context.Background())
	err := r.reconcileOTC(context.Background(), logger, ctrl.Request{}, engine, false)
	assert.NoError(t, err, "CRD absent must not return an error")
}

// -----------------------------------------------------------------------------
// #21 — reconcileOTC CRD-present path (envtest)
// -----------------------------------------------------------------------------

func TestReconcileOTC_CRDPresent(t *testing.T) {
	// envtest does not have the OpenTelemetryCollector CRD installed so this
	// test verifies that the OTC apply returns a recognisable error (no-match)
	// rather than panicking or blocking WAF provisioning. The full CRD-present
	// path is covered by integration tests that install the OTel Operator.
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "eng",
		Namespace:   testNamespace,
		GatewayName: "test-gw",
	})
	require.NoError(t, k8sClient.Create(context.Background(), engine))
	defer func() { _ = k8sClient.Delete(context.Background(), engine) }()

	r := &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		operatorNamespace: testNamespace,
	}

	logger := log.FromContext(context.Background())
	// With CRD present=true but the CRD not actually installed, serverSideApply
	// should return an error (not panic). reconcileOTCBestEffort swallows it;
	// here we call reconcileOTC directly so we can assert the error type.
	err := r.reconcileOTC(context.Background(), logger, ctrl.Request{}, engine, true)
	// We expect an error because the OTC CRD is not in envtest. The key property
	// is that the error is non-nil but not a panic, and WAF provisioning can
	// continue (reconcileOTCBestEffort logs and discards it).
	assert.Error(t, err, "expected an error when OTC CRD is absent from envtest")
}
