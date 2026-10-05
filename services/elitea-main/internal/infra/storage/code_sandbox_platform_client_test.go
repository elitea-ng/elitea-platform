package storage

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

func codePlatformClientFixture(t *testing.T, operation string) (*CodeOwnerGrantSigner, code.PlatformGrantClaims, code.PlatformOwnerResponse) {
	t.Helper()
	s, _, recovery := codeGrantFixture(t)
	c := code.PlatformGrantClaims{Schema: "elitea.sandbox.code-platform-owner-grant.v1", Purpose: "platform_broker_runtime", TenantID: recovery.TenantID, ProjectID: recovery.ProjectID, ExecutionID: recovery.ExecutionID, OriginalGeneration: 1, ClaimID: recovery.ClaimID, ClaimAttempt: recovery.ClaimAttempt, LeaseEpoch: recovery.LeaseEpoch, FenceSHA256: recovery.FenceSHA256, ActivationID: recovery.ActivationID, Attempt: 1, DispatchActivation: recovery.DispatchActivation, JobKey: recovery.JobKey, RequestDigest: strings.Repeat("6", 64), BindingSHA256: recovery.BindingSHA256, PreparedSHA256: strings.Repeat("7", 64), PreparedFingerprint: strings.Repeat("6", 64), PolicySHA256: strings.Repeat("8", 64), MaxCalls: 16, MaxTotalBytes: 1048576, SupervisorAudience: recovery.SupervisorAudience, RequesterWorkloadIdentity: recovery.RequesterWorkloadIdentity, Operation: operation, IssuedAtMillis: 1000, ExpiresAtMillis: 21000}
	r := code.PlatformOwnerResponse{Schema: "elitea.sandbox.code-platform-owner-response.v1", State: "running", Runtime: code.RetainedRuntime{Kind: "docker", RuntimeID: strings.Repeat("9", 64), OwnerEpoch: 4, BindingSHA256: c.BindingSHA256, PreparedSHA256: c.PreparedSHA256, PreparedFingerprint: c.PreparedFingerprint, PolicySHA256: c.PolicySHA256, MaxCalls: c.MaxCalls, MaxTotalBytes: c.MaxTotalBytes, Lifecycle: "dispatched"}}
	return s, c, r
}
func fakeCodePlatformClient(t *testing.T, c code.PlatformGrantClaims, r code.PlatformOwnerResponse) (*CodeOwnerClient, *codeOwnerTestTransport) {
	t.Helper()
	raw, err := code.Canonical(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := &codeOwnerTestTransport{status: 200, body: string(raw)}
	return &CodeOwnerClient{requester: c.RequesterWorkloadIdentity, endpoints: map[string]codeOwnerEndpoint{c.SupervisorAudience: {origin: "https://owner.invalid", client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}}}, tr
}
func TestCodePlatformClientClosedMethodsAndExactReply(t *testing.T) {
	for _, operation := range []string{"read_retained_runtime", "read_pending_platform_call", "publish_committed_platform_reply"} {
		t.Run(operation, func(t *testing.T) {
			s, c, r := codePlatformClientFixture(t, operation)
			reply := []byte("exact signed committed reply")
			if operation == "read_pending_platform_call" {
				frame := base64.RawURLEncoding.EncodeToString([]byte("CP1 bounded original call"))
				r.PendingCallBase64URL = &frame
			}
			if operation == "publish_committed_platform_reply" {
				sequence := uint64(1)
				request := strings.Repeat("a", 64)
				digest := code.Digest(reply)
				c.Sequence = &sequence
				c.PlatformRequestSHA256 = &request
				c.CommittedReplySHA256 = &digest
				published := true
				r.ReplyPublished = &published
			}
			grant, err := s.SignPlatform(c)
			if err != nil {
				t.Fatal(err)
			}
			client, tr := fakeCodePlatformClient(t, c, r)
			switch operation {
			case "read_retained_runtime":
				_, err = client.ReadRetainedRuntime(t.Context(), c, grant)
			case "read_pending_platform_call":
				var frame []byte
				_, frame, err = client.ReadPendingPlatformCall(t.Context(), c, grant)
				if err == nil && !bytes.Equal(frame, []byte("CP1 bounded original call")) {
					t.Fatal("frame changed")
				}
			case "publish_committed_platform_reply":
				_, err = client.PublishCommittedPlatformReply(t.Context(), c, grant, reply)
			}
			if err != nil || tr.calls != 1 || tr.path != "/elitea.runtime.code-platform.v1/jobs/"+c.JobKey+"/"+strings.ReplaceAll(operation, "_", "-") || tr.query != "" {
				t.Fatalf("err=%v attempts=%d path=%s", err, tr.calls, tr.path)
			}
			// Recovery or a different closed operation cannot reuse the same grant.
			before := tr.calls
			wrong := c
			wrong.Operation = "read_retained_runtime"
			if operation == "read_retained_runtime" {
				wrong.Operation = "read_pending_platform_call"
			}
			if _, err = client.platformOperation(t.Context(), wrong, grant, nil); err == nil || tr.calls != before {
				t.Fatal("mismatched signed operation made an owner call")
			}
			if operation == "publish_committed_platform_reply" {
				if _, err = client.PublishCommittedPlatformReply(t.Context(), c, grant, append(reply, '!')); err == nil || tr.calls != before {
					t.Fatal("changed committed bytes published")
				}
			}
		})
	}
}
func TestCodePlatformClientRefusesChangedRuntimeAndForeignResponseOutputs(t *testing.T) {
	for _, name := range []string{"changed policy", "fingerprint substitution", "reserved", "missing runtime", "read reply", "pending on retained read", "false publish", "oversized frame", "missing compiled selector", "redirect"} {
		t.Run(name, func(t *testing.T) {
			operation := "read_retained_runtime"
			if name == "false publish" {
				operation = "publish_committed_platform_reply"
			}
			if name == "oversized frame" {
				operation = "read_pending_platform_call"
			}
			s, c, r := codePlatformClientFixture(t, operation)
			switch name {
			case "changed policy":
				r.Runtime.PolicySHA256 = strings.Repeat("b", 64)
			case "fingerprint substitution":
				r.Runtime.PreparedFingerprint = r.Runtime.PreparedSHA256
			case "reserved":
				r.Runtime.Lifecycle = "reserved"
			case "missing runtime":
				r.Runtime.RuntimeID = ""
			case "read reply":
				b := true
				r.ReplyPublished = &b
			case "pending on retained read":
				frame := base64.RawURLEncoding.EncodeToString([]byte("CP1"))
				r.PendingCallBase64URL = &frame
			case "false publish":
				sequence := uint64(1)
				request := strings.Repeat("c", 64)
				digest := code.Digest([]byte("reply"))
				c.Sequence = &sequence
				c.PlatformRequestSHA256 = &request
				c.CommittedReplySHA256 = &digest
				b := false
				r.ReplyPublished = &b
			case "oversized frame":
				frame := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, code.MaxPlatformCallBytes+1))
				r.PendingCallBase64URL = &frame
			}
			grant, err := s.SignPlatform(c)
			if err != nil {
				t.Fatal(err)
			}
			client, tr := fakeCodePlatformClient(t, c, r)
			if name == "redirect" {
				tr.status = 307
			}
			if name == "missing compiled selector" {
				tr.body = strings.Replace(tr.body, `"compiled_execute":null,`, "", 1)
			}
			if _, err = client.platformOperation(t.Context(), c, grant, func() *string {
				if operation == "publish_committed_platform_reply" {
					s := base64.RawURLEncoding.EncodeToString([]byte("reply"))
					return &s
				}
				return nil
			}()); err == nil || tr.calls != 1 {
				t.Fatal("invalid owner response accepted/retried", err, tr.calls)
			}
		})
	}
}

const codePlatformNotReadyWire = `{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"not_ready","runtime":null,"pending_call_base64url":null,"reply_published":null}`

func TestCodePlatformClientNotReadyIsTypedAndReadOnly(t *testing.T) {
	s, c, r := codePlatformClientFixture(t, "read_retained_runtime")
	grant, err := s.SignPlatform(c)
	if err != nil {
		t.Fatal(err)
	}
	client, tr := fakeCodePlatformClient(t, c, r)
	tr.body = codePlatformNotReadyWire
	runtime, err := client.ReadRetainedRuntime(t.Context(), c, grant)
	if !errors.Is(err, ErrCodePlatformNotReady) || runtime != (code.RetainedRuntime{}) || tr.calls != 1 {
		t.Fatal("typed no-effect observation lost", err, tr.calls)
	}
}
func TestCodePlatformClientNotReadyRefusesMalformedBodiesOperationsAndHTTPRefusals(t *testing.T) {
	cases := []struct {
		name, operation, body string
		status                int
	}{
		{"missing runtime", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, `"runtime":null,`, "", 1), 200},
		{"invented runtime", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, `"runtime":null`, `"runtime":{}`, 1), 200},
		{"pending frame", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, `"pending_call_base64url":null`, `"pending_call_base64url":"Q1Ax"`, 1), 200},
		{"publish permission", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, `"reply_published":null`, `"reply_published":true`, 1), 200},
		{"unknown field", "read_retained_runtime", strings.TrimSuffix(codePlatformNotReadyWire, "}") + `,"runtime_id":"selected"}`, 200},
		{"duplicate state", "read_retained_runtime", strings.TrimSuffix(codePlatformNotReadyWire, "}") + `,"state":"not_ready"}`, 200},
		{"wrong schema", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, "response.v1", "response.v2", 1), 200},
		{"wrong state", "read_retained_runtime", strings.Replace(codePlatformNotReadyWire, "not_ready", "reserved", 1), 200},
		{"pending operation", "read_pending_platform_call", codePlatformNotReadyWire, 200},
		{"publish operation", "publish_committed_platform_reply", codePlatformNotReadyWire, 200},
		{"generic conflict", "read_retained_runtime", codePlatformNotReadyWire, 409},
		{"denied identity", "read_retained_runtime", codePlatformNotReadyWire, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c, r := codePlatformClientFixture(t, tc.operation)
			var reply *string
			if tc.operation == "publish_committed_platform_reply" {
				n := uint64(1)
				request := strings.Repeat("a", 64)
				digest := code.Digest([]byte("reply"))
				c.Sequence = &n
				c.PlatformRequestSHA256 = &request
				c.CommittedReplySHA256 = &digest
				encoded := base64.RawURLEncoding.EncodeToString([]byte("reply"))
				reply = &encoded
			}
			grant, err := s.SignPlatform(c)
			if err != nil {
				t.Fatal(err)
			}
			client, tr := fakeCodePlatformClient(t, c, r)
			tr.body = tc.body
			tr.status = tc.status
			_, err = client.platformOperation(t.Context(), c, grant, reply)
			if err == nil || errors.Is(err, ErrCodePlatformNotReady) || tr.calls != 1 {
				t.Fatal("refusal became not-ready or retried", err, tr.calls)
			}
		})
	}
}

const codePlatformCompletedWire = `{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completed","runtime":null,"pending_call_base64url":null,"reply_published":null}`

func TestCodePlatformClientCompletedIsTypedAndReadOnly(t *testing.T) {
	for _, operation := range []string{"read_retained_runtime", "read_pending_platform_call"} {
		t.Run(operation, func(t *testing.T) {
			s, c, r := codePlatformClientFixture(t, operation)
			grant, err := s.SignPlatform(c)
			if err != nil {
				t.Fatal(err)
			}
			client, tr := fakeCodePlatformClient(t, c, r)
			tr.body = codePlatformCompletedWire
			var runtime code.RetainedRuntime
			var frame []byte
			if operation == "read_retained_runtime" {
				runtime, err = client.ReadRetainedRuntime(t.Context(), c, grant)
			} else {
				runtime, frame, err = client.ReadPendingPlatformCall(t.Context(), c, grant)
			}
			if !errors.Is(err, ErrCodePlatformCompleted) || errors.Is(err, ErrCodePlatformNotReady) || runtime != (code.RetainedRuntime{}) || len(frame) != 0 || tr.calls != 1 {
				t.Fatal("completion acquired runtime authority or frame", runtime, frame, err, tr.calls)
			}
		})
	}
}
func TestCodePlatformClientCompletedRefusesMalformedBodiesOperationsAndHTTPRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"missing schema", strings.Replace(codePlatformCompletedWire, `"schema":"elitea.sandbox.code-platform-owner-response.v1",`, "", 1), 200},
		{"missing state", strings.Replace(codePlatformCompletedWire, `"state":"completed",`, "", 1), 200},
		{"missing runtime", strings.Replace(codePlatformCompletedWire, `"runtime":null,`, "", 1), 200},
		{"missing pending", strings.Replace(codePlatformCompletedWire, `"pending_call_base64url":null,`, "", 1), 200},
		{"missing publication", strings.Replace(codePlatformCompletedWire, `,"reply_published":null`, "", 1), 200},
		{"invented runtime", strings.Replace(codePlatformCompletedWire, `"runtime":null`, `"runtime":{}`, 1), 200},
		{"pending frame", strings.Replace(codePlatformCompletedWire, `"pending_call_base64url":null`, `"pending_call_base64url":"Q1Ax"`, 1), 200},
		{"publication true", strings.Replace(codePlatformCompletedWire, `"reply_published":null`, `"reply_published":true`, 1), 200},
		{"publication false", strings.Replace(codePlatformCompletedWire, `"reply_published":null`, `"reply_published":false`, 1), 200},
		{"wrong schema", strings.Replace(codePlatformCompletedWire, "response.v1", "response.v2", 1), 200},
		{"wrong state", strings.Replace(codePlatformCompletedWire, "completed", "cancelled", 1), 200},
		{"unknown field", strings.TrimSuffix(codePlatformCompletedWire, "}") + `,"result":{}}`, 200},
		{"duplicate state", strings.TrimSuffix(codePlatformCompletedWire, "}") + `,"state":"completed"}`, 200},
		{"trailing data", codePlatformCompletedWire + `{}`, 200},
		{"malformed", strings.TrimSuffix(codePlatformCompletedWire, "}"), 200},
		{"generic conflict", codePlatformCompletedWire, 409},
		{"denied identity", codePlatformCompletedWire, 403},
		{"missing owner", codePlatformCompletedWire, 404},
	}
	for _, operation := range []string{"read_retained_runtime", "read_pending_platform_call"} {
		for _, tc := range cases {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				s, c, r := codePlatformClientFixture(t, operation)
				grant, err := s.SignPlatform(c)
				if err != nil {
					t.Fatal(err)
				}
				client, tr := fakeCodePlatformClient(t, c, r)
				tr.body = tc.body
				tr.status = tc.status
				_, err = client.platformOperation(t.Context(), c, grant, nil)
				if err == nil || errors.Is(err, ErrCodePlatformCompleted) || errors.Is(err, ErrCodePlatformNotReady) || tr.calls != 1 {
					t.Fatal("refusal became completion or retried", err, tr.calls)
				}
			})
		}
	}
	t.Run("publish operation", func(t *testing.T) {
		s, c, r := codePlatformClientFixture(t, "publish_committed_platform_reply")
		reply := []byte("reply")
		n := uint64(1)
		request := strings.Repeat("a", 64)
		digest := code.Digest(reply)
		c.Sequence = &n
		c.PlatformRequestSHA256 = &request
		c.CommittedReplySHA256 = &digest
		grant, err := s.SignPlatform(c)
		if err != nil {
			t.Fatal(err)
		}
		client, tr := fakeCodePlatformClient(t, c, r)
		tr.body = codePlatformCompletedWire
		runtime, err := client.PublishCommittedPlatformReply(t.Context(), c, grant, reply)
		if !errors.Is(err, code.ErrRejected) || errors.Is(err, ErrCodePlatformCompleted) || runtime != (code.RetainedRuntime{}) || tr.calls != 1 {
			t.Fatal("completed response authorized publication", runtime, err, tr.calls)
		}
	})
}

const codePlatformCompletingWire = `{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completing","runtime":null,"pending_call_base64url":null,"reply_published":null}`

func TestCodePlatformClientCompletingIsTypedAndReadOnly(t *testing.T) {
	for _, operation := range []string{"read_retained_runtime", "read_pending_platform_call"} {
		t.Run(operation, func(t *testing.T) {
			s, c, r := codePlatformClientFixture(t, operation)
			grant, err := s.SignPlatform(c)
			if err != nil {
				t.Fatal(err)
			}
			client, tr := fakeCodePlatformClient(t, c, r)
			tr.body = codePlatformCompletingWire
			var runtime code.RetainedRuntime
			var frame []byte
			if operation == "read_retained_runtime" {
				runtime, err = client.ReadRetainedRuntime(t.Context(), c, grant)
			} else {
				runtime, frame, err = client.ReadPendingPlatformCall(t.Context(), c, grant)
			}
			if !errors.Is(err, ErrCodePlatformCompleting) || errors.Is(err, ErrCodePlatformNotReady) || runtime != (code.RetainedRuntime{}) || len(frame) != 0 || tr.calls != 1 {
				t.Fatal("completion acquired runtime authority or frame", runtime, frame, err, tr.calls)
			}
		})
	}
}
func TestCodePlatformClientCompletingRefusesMalformedBodiesOperationsAndHTTPRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"missing schema", strings.Replace(codePlatformCompletingWire, `"schema":"elitea.sandbox.code-platform-owner-response.v1",`, "", 1), 200},
		{"missing state", strings.Replace(codePlatformCompletingWire, `"state":"completing",`, "", 1), 200},
		{"missing runtime", strings.Replace(codePlatformCompletingWire, `"runtime":null,`, "", 1), 200},
		{"missing pending", strings.Replace(codePlatformCompletingWire, `"pending_call_base64url":null,`, "", 1), 200},
		{"missing publication", strings.Replace(codePlatformCompletingWire, `,"reply_published":null`, "", 1), 200},
		{"invented runtime", strings.Replace(codePlatformCompletingWire, `"runtime":null`, `"runtime":{}`, 1), 200},
		{"pending frame", strings.Replace(codePlatformCompletingWire, `"pending_call_base64url":null`, `"pending_call_base64url":"Q1Ax"`, 1), 200},
		{"publication true", strings.Replace(codePlatformCompletingWire, `"reply_published":null`, `"reply_published":true`, 1), 200},
		{"publication false", strings.Replace(codePlatformCompletingWire, `"reply_published":null`, `"reply_published":false`, 1), 200},
		{"wrong schema", strings.Replace(codePlatformCompletingWire, "response.v1", "response.v2", 1), 200},
		{"wrong state", strings.Replace(codePlatformCompletingWire, "completing", "cancelled", 1), 200},
		{"unknown field", strings.TrimSuffix(codePlatformCompletingWire, "}") + `,"result":{}}`, 200},
		{"duplicate state", strings.TrimSuffix(codePlatformCompletingWire, "}") + `,"state":"completing"}`, 200},
		{"trailing data", codePlatformCompletingWire + `{}`, 200},
		{"malformed", strings.TrimSuffix(codePlatformCompletingWire, "}"), 200},
		{"generic conflict", codePlatformCompletingWire, 409},
		{"denied identity", codePlatformCompletingWire, 403},
		{"missing owner", codePlatformCompletingWire, 404},
	}
	for _, operation := range []string{"read_retained_runtime", "read_pending_platform_call"} {
		for _, tc := range cases {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				s, c, r := codePlatformClientFixture(t, operation)
				grant, err := s.SignPlatform(c)
				if err != nil {
					t.Fatal(err)
				}
				client, tr := fakeCodePlatformClient(t, c, r)
				tr.body = tc.body
				tr.status = tc.status
				_, err = client.platformOperation(t.Context(), c, grant, nil)
				if err == nil || errors.Is(err, ErrCodePlatformCompleting) || errors.Is(err, ErrCodePlatformNotReady) || tr.calls != 1 {
					t.Fatal("refusal became completion or retried", err, tr.calls)
				}
			})
		}
	}
	t.Run("publish operation", func(t *testing.T) {
		s, c, r := codePlatformClientFixture(t, "publish_committed_platform_reply")
		reply := []byte("reply")
		n := uint64(1)
		request := strings.Repeat("a", 64)
		digest := code.Digest(reply)
		c.Sequence = &n
		c.PlatformRequestSHA256 = &request
		c.CommittedReplySHA256 = &digest
		grant, err := s.SignPlatform(c)
		if err != nil {
			t.Fatal(err)
		}
		client, tr := fakeCodePlatformClient(t, c, r)
		tr.body = codePlatformCompletingWire
		runtime, err := client.PublishCommittedPlatformReply(t.Context(), c, grant, reply)
		if !errors.Is(err, code.ErrRejected) || errors.Is(err, ErrCodePlatformCompleting) || runtime != (code.RetainedRuntime{}) || tr.calls != 1 {
			t.Fatal("completed response authorized publication", runtime, err, tr.calls)
		}
	})
}
