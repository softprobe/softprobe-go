package softprobe

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureCase returns a canonical single-span OTLP case JSON suitable for
// testing FindInCase and ResponseFromSpan.
func captureCase() string {
	return `{
  "version": "1.0.0",
  "caseId": "fragment-happy-path",
  "traces": [{
    "resourceSpans": [{
      "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "fragment-svc"}}]},
      "scopeSpans": [{"spans": [
        {"spanId": "span-1", "attributes": [
          {"key": "sp.span.type", "value": {"stringValue": "inject"}},
          {"key": "sp.traffic.direction", "value": {"stringValue": "outbound"}},
          {"key": "http.request.method", "value": {"stringValue": "GET"}},
          {"key": "url.path", "value": {"stringValue": "/fragment"}},
          {"key": "url.host", "value": {"stringValue": "fragment.internal"}},
          {"key": "http.response.status_code", "value": {"intValue": 200}},
          {"key": "http.response.header.content-type", "value": {"stringValue": "application/json"}},
          {"key": "http.response.body", "value": {"stringValue": "{\"dep\":\"ok\"}"}}
        ]}
      ]}]
    }]
  }]
}`
}

// recordingTransport captures every outbound request and returns a
// predictable session-shaped response.
type recordingTransport struct {
	calls     []recordedCall
	sessionID string
	revision  int
}

type recordedCall struct {
	method string
	path   string
	body   string
}

func (t *recordingTransport) Do(req *http.Request) (*http.Response, error) {
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.calls = append(t.calls, recordedCall{method: req.Method, path: req.URL.Path, body: string(data)})

	if t.sessionID == "" {
		t.sessionID = "sess_123"
	}

	if strings.HasSuffix(req.URL.Path, "/close") {
		body := `{"sessionId":"` + t.sessionID + `","closed":true}`
		return makeResponse(200, body), nil
	}
	t.revision++
	body := `{"sessionId":"` + t.sessionID + `","sessionRevision":` + itoa(t.revision) + `}`
	return makeResponse(200, body), nil
}

func makeResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func writeCaseFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "case.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write case file: %v", err)
	}
	return path
}

func newSoftprobe(t *transportFixture) *Softprobe {
	return New(Options{BaseURL: "http://runtime.test", Transport: t.transport})
}

type transportFixture struct{ transport *recordingTransport }

func newFixture() *transportFixture { return &transportFixture{transport: &recordingTransport{}} }

func TestStartSessionPostsCreate(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)

	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.ID() != "sess_123" {
		t.Fatalf("session id = %q, want sess_123", session.ID())
	}
	if n := len(fx.transport.calls); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	call := fx.transport.calls[0]
	if call.method != http.MethodPost || call.path != "/v1/sessions" {
		t.Fatalf("unexpected call: %+v", call)
	}
	if call.body != `{"mode":"replay"}` {
		t.Fatalf("create body = %q", call.body)
	}
}

func TestAttachReusesSessionIDWithoutHTTP(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)

	session := sp.Attach("sess_existing")
	if session.ID() != "sess_existing" {
		t.Fatalf("session id = %q", session.ID())
	}
	if n := len(fx.transport.calls); n != 0 {
		t.Fatalf("Attach must not issue HTTP, got %d calls", n)
	}
}

func TestLoadCaseFromFilePostsAndEnablesFindInCase(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	path := writeCaseFile(t, captureCase())
	if err := session.LoadCaseFromFile(path); err != nil {
		t.Fatalf("LoadCaseFromFile: %v", err)
	}

	if n := len(fx.transport.calls); n != 2 {
		t.Fatalf("calls = %d, want 2 (create + load-case)", n)
	}
	loadCall := fx.transport.calls[1]
	if !strings.HasSuffix(loadCall.path, "/load-case") {
		t.Fatalf("load call path = %q", loadCall.path)
	}
	if loadCall.body != captureCase() {
		t.Fatalf("load-case body mismatch")
	}
}

func TestSetPolicyPostsToSessionPolicy(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	policy := []byte(`{"externalHttp":"strict"}`)
	if err := session.SetPolicy(policy); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if n := len(fx.transport.calls); n != 2 {
		t.Fatalf("calls = %d, want 2 (create + policy)", n)
	}
	policyCall := fx.transport.calls[1]
	if policyCall.method != http.MethodPost || !strings.HasSuffix(policyCall.path, "/policy") {
		t.Fatalf("policy call = %+v", policyCall)
	}
	if policyCall.body != string(policy) {
		t.Fatalf("policy body = %q", policyCall.body)
	}
}

func TestFindInCaseReturnsCapturedResponseForSingleMatch(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	session.LoadCaseFromFile(writeCaseFile(t, captureCase()))

	hit, err := session.FindInCase(CaseSpanPredicate{
		Direction: "outbound", Method: "GET", Path: "/fragment",
	})
	if err != nil {
		t.Fatalf("FindInCase: %v", err)
	}
	if hit.Response.Status != 200 {
		t.Fatalf("status = %d", hit.Response.Status)
	}
	if hit.Response.Body != `{"dep":"ok"}` {
		t.Fatalf("body = %q", hit.Response.Body)
	}
	if hit.Response.Headers["content-type"] != "application/json" {
		t.Fatalf("headers = %+v", hit.Response.Headers)
	}
	if hit.Span == nil {
		t.Fatal("span must be non-nil")
	}
}

func TestFindInCaseRequiresLoadedCase(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")

	_, err := session.FindInCase(CaseSpanPredicate{Path: "/fragment"})
	if err == nil || !strings.Contains(err.Error(), "LoadCaseFromFile") {
		t.Fatalf("expected LoadCaseFromFile error, got %v", err)
	}
}

func TestFindInCaseThrowsWhenNoSpanMatches(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	session.LoadCaseFromFile(writeCaseFile(t, captureCase()))

	_, err := session.FindInCase(CaseSpanPredicate{Path: "/missing"})
	if err == nil || !strings.Contains(err.Error(), "no span") || !strings.Contains(err.Error(), "/missing") {
		t.Fatalf("expected no-span error, got %v", err)
	}
}

func TestFindInCaseThrowsWithCandidateIDsOnMultiMatch(t *testing.T) {
	ambiguous := `{"traces":[{"resourceSpans":[{"scopeSpans":[{"spans":[
    {"spanId":"a1","attributes":[
      {"key":"sp.span.type","value":{"stringValue":"inject"}},
      {"key":"http.request.method","value":{"stringValue":"GET"}},
      {"key":"url.path","value":{"stringValue":"/dup"}},
      {"key":"http.response.status_code","value":{"intValue":200}}
    ]},
    {"spanId":"a2","attributes":[
      {"key":"sp.span.type","value":{"stringValue":"inject"}},
      {"key":"http.request.method","value":{"stringValue":"GET"}},
      {"key":"url.path","value":{"stringValue":"/dup"}},
      {"key":"http.response.status_code","value":{"intValue":200}}
    ]}
  ]}]}]}]}`

	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	session.LoadCaseFromFile(writeCaseFile(t, ambiguous))

	_, err := session.FindInCase(CaseSpanPredicate{Path: "/dup"})
	if err == nil {
		t.Fatal("expected ambiguous match error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2 spans match") || !strings.Contains(msg, "a1") || !strings.Contains(msg, "a2") {
		t.Fatalf("expected candidate-id error, got %q", msg)
	}
}

func TestFindInCaseSupportsHTTP2PseudoHeaderFallbacks(t *testing.T) {
	pseudo := `{"traces":[{"resourceSpans":[{"scopeSpans":[{"spans":[
    {"spanId":"p1","attributes":[
      {"key":"sp.span.type","value":{"stringValue":"inject"}},
      {"key":"http.request.header.:method","value":{"stringValue":"GET"}},
      {"key":"http.request.header.:path","value":{"stringValue":"/fragment"}},
      {"key":"http.response.status_code","value":{"intValue":204}}
    ]}
  ]}]}]}]}`

	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	session.LoadCaseFromFile(writeCaseFile(t, pseudo))

	hit, err := session.FindInCase(CaseSpanPredicate{Method: "GET", Path: "/fragment"})
	if err != nil {
		t.Fatalf("FindInCase: %v", err)
	}
	if hit.Response.Status != 204 {
		t.Fatalf("status = %d, want 204", hit.Response.Status)
	}
}

func TestMockOutboundPostsRulesAsPartOfRuleSet(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	session.LoadCaseFromFile(writeCaseFile(t, captureCase()))

	hit, err := session.FindInCase(CaseSpanPredicate{Direction: "outbound", Method: "GET", Path: "/fragment"})
	if err != nil {
		t.Fatalf("FindInCase: %v", err)
	}
	priority := 100
	if err := session.MockOutbound(MockRuleSpec{
		ID:        "fragment-replay",
		Priority:  &priority,
		Direction: "outbound",
		Method:    "GET",
		Path:      "/fragment",
		Response:  hit.Response,
	}); err != nil {
		t.Fatalf("MockOutbound: %v", err)
	}

	if n := len(fx.transport.calls); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
	rulesCall := fx.transport.calls[2]
	if !strings.HasSuffix(rulesCall.path, "/rules") || rulesCall.method != http.MethodPost {
		t.Fatalf("rules call = %+v", rulesCall)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(rulesCall.body), &body); err != nil {
		t.Fatalf("decode rules body: %v", err)
	}
	if v, _ := body["version"].(float64); v != 1 {
		t.Fatalf("version = %v", body["version"])
	}
	rules, _ := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules len = %d", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	if rule["id"] != "fragment-replay" {
		t.Fatalf("rule.id = %v", rule["id"])
	}
	if p, _ := rule["priority"].(float64); int(p) != 100 {
		t.Fatalf("rule.priority = %v", rule["priority"])
	}
	when, _ := rule["when"].(map[string]any)
	if when["direction"] != "outbound" || when["method"] != "GET" || when["path"] != "/fragment" {
		t.Fatalf("when = %+v", when)
	}
	then, _ := rule["then"].(map[string]any)
	if then["action"] != "mock" {
		t.Fatalf("then.action = %v", then["action"])
	}
	response, _ := then["response"].(map[string]any)
	if int(response["status"].(float64)) != 200 {
		t.Fatalf("response.status = %v", response["status"])
	}
	if response["body"] != `{"dep":"ok"}` {
		t.Fatalf("response.body = %v", response["body"])
	}
}

func TestClearRulesPostsEmptyRuleSet(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	if err := session.ClearRules(); err != nil {
		t.Fatalf("ClearRules: %v", err)
	}

	if n := len(fx.transport.calls); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	rulesCall := fx.transport.calls[1]
	if !strings.HasSuffix(rulesCall.path, "/rules") {
		t.Fatalf("path = %q", rulesCall.path)
	}
	if rulesCall.body != `{"rules":[],"version":1}` && rulesCall.body != `{"version":1,"rules":[]}` {
		t.Fatalf("clear body = %q", rulesCall.body)
	}
}

func TestClosePostsSessionClose(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	last := fx.transport.calls[len(fx.transport.calls)-1]
	if !strings.HasSuffix(last.path, "/close") {
		t.Fatalf("close path = %q", last.path)
	}
	if last.body != "{}" {
		t.Fatalf("close body = %q", last.body)
	}
}

func TestRuntimeErrorSurfacesStatusAndBody(t *testing.T) {
	failingTransport := &failingTransport{status: 404, body: `{"error":"unknown session"}`}
	sp := New(Options{BaseURL: "http://runtime.test", Transport: failingTransport})

	_, err := sp.StartSession("replay")
	if err == nil {
		t.Fatal("expected error")
	}
	re, ok := err.(*RuntimeError)
	if !ok {
		t.Fatalf("error type = %T, want *RuntimeError", err)
	}
	if re.StatusCode != 404 || !strings.Contains(re.Body, "unknown session") {
		t.Fatalf("unexpected RuntimeError: %+v", re)
	}
}

type failingTransport struct {
	status int
	body   string
}

func (t *failingTransport) Do(_ *http.Request) (*http.Response, error) {
	return makeResponse(t.status, t.body), nil
}
