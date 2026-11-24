package pipelinelimits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const validHTTPPipeline = `entry_point: enrich_fx
state:
  run_id: str
  batch_no: int
  fx: dict
nodes:
  - id: enrich_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request:
      path: {run_id: {from: run_id}, batch_no: {from: batch_no}}
    response: {mode: json, accepted_statuses: [{first: 200, last: 200}], max_bytes: 16384}
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 1000
      timeout_ms: 5000
    input: [run_id, batch_no]
    output: [fx]
    transition: END
`

func TestHTTPNodeSchemaAndSettingsAdmission(t *testing.T) {
	if err := pipelinelimits.Check(validHTTPPipeline); err != nil {
		t.Fatalf("valid HTTP pipeline refused: %v", err)
	}
	ops, err := pipelinelimits.HTTPNodeOperations(validHTTPPipeline)
	if err != nil || len(ops) != 1 || ops[0].NodeID != "enrich_fx" || ops[0].Operation != "fx_reference.rates" {
		t.Fatalf("HTTPNodeOperations = %+v, %v", ops, err)
	}
	// Timeout at 30000 ms accepted; 30001 ms refused with http_timeout_range.
	atCap := strings.Replace(validHTTPPipeline, "timeout_ms: 5000", "timeout_ms: 30000", 1)
	if err := pipelinelimits.Check(atCap); err != nil {
		t.Fatalf("30000ms refused: %v", err)
	}
	overCap := strings.Replace(validHTTPPipeline, "timeout_ms: 5000", "timeout_ms: 30001", 1)
	api := pipelinelimits.Refusal(pipelinelimits.Check(overCap))
	if api == nil || api.Code != pipelinelimits.CodeNodeSettingsInvalid || !strings.Contains(api.Message, "30000") {
		t.Fatalf("30001ms got %v", api)
	}
}

func TestHTTPNodeInvalidSchemasRefusedAtSave(t *testing.T) {
	cases := map[string]string{
		"wrong revision":        strings.Replace(validHTTPPipeline, "revision: 2", "revision: 1", 1),
		"url in yaml":           validHTTPPipeline + "    url: https://example.com\n",
		"method in yaml":        validHTTPPipeline + "    method: GET\n",
		"bad operation":         strings.Replace(validHTTPPipeline, "operation: fx_reference.rates", "operation: Bad/Op", 1),
		"3xx status":            strings.Replace(validHTTPPipeline, "first: 200, last: 200", "first: 300, last: 302", 1),
		"restricted hdr":        strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: Authorization, from: run_id}]", 1),
		"x-forwarded-for hdr":   strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Forwarded-For, from: run_id}]", 1),
		"forwarded hdr":         strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: Forwarded, from: run_id}]", 1),
		"x-auth hdr":            strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Auth-User, from: run_id}]", 1),
		"duplicate hdr names":   strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Trace, value: a}, {name: x-trace, value: b}]", 1),
		"int literal hdr":       strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Count, value: 42}]", 1),
		"ctl char literal hdr":  strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Trace, value: \"bad\\x07val\"}]", 1),
		"unicode sep hdr":       strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "headers: [{name: X-Trace, value: \"bad\u2028val\"}]", 1),
		"dup key before type":   "entry_point: enrich_fx\nstate:\n  fx: dict\nnodes:\n  - id: enrich_fx\n    operation: fx_reference.rates\n    operation: other.op\n    type: http\n    revision: 2\n    request: {}\n    response: {mode: json}\n    output: [fx]\n",
		"dup nested req key":    strings.Replace(validHTTPPipeline, "path: {run_id: {from: run_id}, batch_no: {from: batch_no}}", "path: {run_id: {from: run_id}}\n      path: {batch_no: {from: batch_no}}", 1),
		"two outputs":           strings.Replace(validHTTPPipeline, "output: [fx]", "output: [fx, extra]", 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			api := pipelinelimits.Refusal(pipelinelimits.Check(doc))
			if api == nil || api.Code != pipelinelimits.CodeHTTPNodeInvalid {
				t.Fatalf("%s: got %v, want %s", name, api, pipelinelimits.CodeHTTPNodeInvalid)
			}
			if !strings.Contains(api.Message, `Node "enrich_fx"`) {
				t.Fatalf("%s: message %q does not name the node", name, api.Message)
			}
		})
	}

	t.Run("yaml merge key supplying http node is refused", func(t *testing.T) {
		mergeDoc := "base: &http_base\n  type: http\n  revision: 2\n  operation: fx_reference.rates\n  request: {}\n  response: {mode: json}\n  output: [fx]\nentry_point: enrich_fx\nstate:\n  fx: dict\nnodes:\n  - id: enrich_fx\n    <<: *http_base\n"
		api := pipelinelimits.Refusal(pipelinelimits.Check(mergeDoc))
		if api == nil || api.Code != pipelinelimits.CodeHTTPNodeInvalid {
			t.Fatalf("merge check got %v, want %s", api, pipelinelimits.CodeHTTPNodeInvalid)
		}
		if _, err := pipelinelimits.HTTPNodeOperations(mergeDoc); pipelinelimits.Refusal(err) == nil {
			t.Fatalf("HTTPNodeOperations should refuse merge key introducing http node, got %v", err)
		}
	})
}

func TestHTTPNodeRefusedInsideParallelAndMapFanoutChildren(t *testing.T) {
	parallelDoc := `entry_point: gather
state:
  joined: list
  out: dict
nodes:
  - id: gather
    type: parallel
    branches: [{id: one, node: call_http}]
    max_concurrency: 2
    wait: all
    output: [joined]
    transition: END
  - id: call_http
    type: http
    revision: 2
    operation: sample.get
    request: {}
    response: {mode: json}
    output: [out]
`
	var api *apierr.APIError
	if err := pipelinelimits.Check(parallelDoc); !errors.As(err, &api) || api.Code != pipelinelimits.CodeParallelBranchUnsupported {
		t.Fatalf("parallel http: got %v", err)
	}
	if !strings.Contains(api.Message, `node "call_http"`) || !strings.Contains(api.Message, "5d-3") {
		t.Fatalf("parallel http message %q", api.Message)
	}

	mapDoc := `entry_point: fan
state:
  items: list
  results: list
  out: dict
nodes:
  - id: fan
    type: map
    source: items
    worker: call_http
    item: item
    index: idx
    outputs: [out]
    destination: results
    transition: END
  - id: call_http
    type: http
    revision: 2
    operation: sample.get
    request: {}
    response: {mode: json}
    output: [out]
`
	api = nil
	if err := pipelinelimits.Check(mapDoc); !errors.As(err, &api) || api.Code != pipelinelimits.CodeMapFormUnsupported {
		t.Fatalf("map http: got %v", err)
	}
	if !strings.Contains(api.Message, `node "call_http"`) || !strings.Contains(api.Message, "5d-3") {
		t.Fatalf("map http message %q", api.Message)
	}
}
