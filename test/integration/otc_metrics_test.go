//go:build integration

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

package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

func assertMetricPresent(t *testing.T, httpc *http.Client, url, metricName string) {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		resp, err := httpc.Get(url)
		if !assert.NoError(collect, err) {
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(collect, http.StatusOK, resp.StatusCode)
		assert.Contains(collect, string(body), metricName, "expected %s in OTC metrics", metricName)
	}, framework.WasmEnforcementTimeout, 5*time.Second)
}

// TestOTCMetricsPipeline verifies that the OTC sidecar's log→metrics pipeline
// produces coraza_waf_requests_total at its Prometheus endpoint after WAF
// traffic flows through the Gateway.
//
// Requires: OTel Operator installed on the KIND cluster.
func TestOTCMetricsPipeline(t *testing.T) {
	if os.Getenv("CORAZA_WASM_JSON_EVENTS") == "" {
		t.Skip("skipping: WASM plugin does not yet emit coraza_waf_request JSON events (set CORAZA_WASM_JSON_EVENTS=true to enable)")
	}
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("otc-metrics")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy WAF rules")
	s.CreateRuleSource(ns, "base", `SecRuleEngine DetectionOnly`)
	s.CreateRuleSet(ns, "ruleset", []string{"base"}, nil)

	s.Step("create engine targeting gateway")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	s.Step("send traffic through gateway")
	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectAllowed("/")

	// Send several requests to ensure log events are generated.
	for range 5 {
		gw.Get("/")
	}

	s.Step("port-forward to OTC sidecar prometheus endpoint")
	gwSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", "gw")
	otcProxy := s.ProxyToPod(ns, gwSelector, 9090)
	otcMetricsURL := fmt.Sprintf("http://localhost:%s/metrics", otcProxy.LocalPort())

	httpc := &http.Client{Timeout: 10 * time.Second}

	s.Step("verify coraza_waf_requests_total appears")
	assertMetricPresent(t, httpc, otcMetricsURL, "coraza_waf_requests_total")
}

// TestOTCBlockedMetrics verifies that coraza_waf_blocked_requests_total
// appears after traffic is blocked by a WAF rule.
//
// Requires: OTel Operator installed on the KIND cluster.
func TestOTCBlockedMetrics(t *testing.T) {
	if os.Getenv("CORAZA_WASM_JSON_EVENTS") == "" {
		t.Skip("skipping: WASM plugin does not yet emit coraza_waf_* JSON events (set CORAZA_WASM_JSON_EVENTS=true to enable)")
	}
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("otc-blocked")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy WAF rules that block requests")
	s.CreateRuleSource(ns, "base", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block", framework.SimpleBlockRule(9001, "blocked-test"))
	s.CreateRuleSet(ns, "ruleset", []string{"base", "block"}, nil)

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	s.Step("send blocked traffic")
	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=blocked-test")
	for range 5 {
		gw.Get("/?test=blocked-test")
	}

	s.Step("port-forward to OTC sidecar")
	gwSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", "gw")
	otcProxy := s.ProxyToPod(ns, gwSelector, 9090)
	otcMetricsURL := fmt.Sprintf("http://localhost:%s/metrics", otcProxy.LocalPort())

	httpc := &http.Client{Timeout: 10 * time.Second}

	s.Step("verify requests_total metric appears")
	assertMetricPresent(t, httpc, otcMetricsURL, "coraza_waf_requests_total")

	s.Step("verify blocked_requests_total metric appears")
	assertMetricPresent(t, httpc, otcMetricsURL, "coraza_waf_blocked_requests_total")
}
