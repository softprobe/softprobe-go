package softprobe

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreachableTransport simulates a network-layer failure (connection refused,
// DNS error, timeout). *UnreachableError is the stable surface the SDK must
// raise in this case.
type unreachableTransport struct{ err error }

func (t *unreachableTransport) Do(_ *http.Request) (*http.Response, error) {
	return nil, t.err
}

// unknownSessionTransport answers /v1/sessions once (to allow StartSession to
// succeed) and then returns the stable unknown_session envelope for every
// subsequent call.
type unknownSessionTransport struct {
	calls int
}

func (t *unknownSessionTransport) Do(req *http.Request) (*http.Response, error) {
	t.calls++
	if req.URL.Path == "/v1/sessions" {
		return makeResponse(200, `{"sessionId":"sess_missing","sessionRevision":0}`), nil
	}
	body := `{"error":{"code":"unknown_session","message":"session not found"}}`
	return makeResponse(404, body), nil
}

func TestLoadCaseAcceptsAPreparedJSONDocument(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := session.LoadCase([]byte(captureCase())); err != nil {
		t.Fatalf("LoadCase: %v", err)
	}

	if n := len(fx.transport.calls); n != 2 {
		t.Fatalf("calls = %d, want 2 (create + load-case)", n)
	}
	load := fx.transport.calls[1]
	if !strings.HasSuffix(load.path, "/load-case") {
		t.Fatalf("load path = %q", load.path)
	}
	if load.body != captureCase() {
		t.Fatalf("load body mismatch")
	}

	hit, err := session.FindInCase(CaseSpanPredicate{Direction: "outbound", Path: "/fragment"})
	if err != nil {
		t.Fatalf("FindInCase after LoadCase: %v", err)
	}
	if hit.Response.Status != 200 {
		t.Fatalf("response status = %d", hit.Response.Status)
	}
}

func TestFindAllInCaseReturnsEveryMatch(t *testing.T) {
	doubled := `{"traces":[{"resourceSpans":[{"scopeSpans":[{"spans":[
    {"spanId":"a1","attributes":[
      {"key":"sp.span.type","value":{"stringValue":"inject"}},
      {"key":"sp.traffic.direction","value":{"stringValue":"outbound"}},
      {"key":"http.request.method","value":{"stringValue":"GET"}},
      {"key":"url.path","value":{"stringValue":"/dup"}},
      {"key":"http.response.status_code","value":{"intValue":200}}
    ]},
    {"spanId":"a2","attributes":[
      {"key":"sp.span.type","value":{"stringValue":"extract"}},
      {"key":"sp.traffic.direction","value":{"stringValue":"outbound"}},
      {"key":"http.request.method","value":{"stringValue":"GET"}},
      {"key":"url.path","value":{"stringValue":"/dup"}},
      {"key":"http.response.status_code","value":{"intValue":201}}
    ]}
  ]}]}]}]}`

	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")
	if err := session.LoadCase([]byte(doubled)); err != nil {
		t.Fatalf("LoadCase: %v", err)
	}

	hits, err := session.FindAllInCase(CaseSpanPredicate{Direction: "outbound", Path: "/dup"})
	if err != nil {
		t.Fatalf("FindAllInCase: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
}

func TestSetAuthFixturesPostsToFixturesAuth(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")

	fixtures := []byte(`{"tokens":["t1"]}`)
	if err := session.SetAuthFixtures(fixtures); err != nil {
		t.Fatalf("SetAuthFixtures: %v", err)
	}

	last := fx.transport.calls[len(fx.transport.calls)-1]
	if !strings.HasSuffix(last.path, "/fixtures/auth") {
		t.Fatalf("fixtures path = %q", last.path)
	}
	if last.body != string(fixtures) {
		t.Fatalf("fixtures body = %q", last.body)
	}
}

func TestRuntimeUnreachableSurfacesTypedError(t *testing.T) {
	sp := New(Options{
		BaseURL:   "http://runtime.test",
		Transport: &unreachableTransport{err: errors.New("connect ECONNREFUSED")},
	})

	_, err := sp.StartSession("replay")
	if err == nil {
		t.Fatal("expected unreachable error")
	}
	var ue *UnreachableError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *UnreachableError", err)
	}
	if !strings.Contains(ue.Error(), "ECONNREFUSED") {
		t.Fatalf("unexpected UnreachableError: %v", ue)
	}
}

func TestUnknownSessionSurfacesTypedError(t *testing.T) {
	transport := &unknownSessionTransport{}
	sp := New(Options{BaseURL: "http://runtime.test", Transport: transport})
	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	err = session.Close()
	if err == nil {
		t.Fatal("expected unknown-session error")
	}
	var use *UnknownSessionError
	if !errors.As(err, &use) {
		t.Fatalf("error type = %T, want *UnknownSessionError", err)
	}
	if use.StatusCode != 404 {
		t.Fatalf("StatusCode = %d", use.StatusCode)
	}
	// Should still be classifiable as *RuntimeError for callers that only
	// inspect HTTP-status-carrying errors.
	var re *RuntimeError
	if !errors.As(err, &re) || re.StatusCode != 404 {
		t.Fatalf("expected RuntimeError match, got %T", err)
	}
}

func TestCaseLoadErrorWrapsFileAndParseFailures(t *testing.T) {
	fx := newFixture()
	sp := newSoftprobe(fx)
	session, _ := sp.StartSession("replay")

	// Missing file.
	err := session.LoadCaseFromFile(filepath.Join(t.TempDir(), "missing.json"))
	var cle *CaseLoadError
	if err == nil || !errors.As(err, &cle) {
		t.Fatalf("expected *CaseLoadError for missing file, got %T %v", err, err)
	}

	// Invalid JSON.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if werr := os.WriteFile(bad, []byte(`{"version":`), 0o600); werr != nil {
		t.Fatalf("write bad file: %v", werr)
	}
	err = session.LoadCaseFromFile(bad)
	if err == nil || !errors.As(err, &cle) {
		t.Fatalf("expected *CaseLoadError for bad JSON, got %T %v", err, err)
	}

	// Invalid JSON passed directly to LoadCase.
	err = session.LoadCase([]byte(`not json`))
	if err == nil || !errors.As(err, &cle) {
		t.Fatalf("expected *CaseLoadError for bad JSON in LoadCase, got %T %v", err, err)
	}
}

func TestCaseLookupAmbiguityIsTypedError(t *testing.T) {
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
	if err := session.LoadCase([]byte(ambiguous)); err != nil {
		t.Fatalf("LoadCase: %v", err)
	}

	_, err := session.FindInCase(CaseSpanPredicate{Path: "/dup"})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	var ae *CaseLookupAmbiguityError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *CaseLookupAmbiguityError", err)
	}
	if !strings.Contains(ae.Error(), "a1") || !strings.Contains(ae.Error(), "a2") {
		t.Fatalf("ambiguity error lost span ids: %v", ae)
	}
}
