package softprobe

import (
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// PD7.3d — Go SDK parity test.
//
// Drives the full facade (StartSession → LoadCaseFromFile → FindInCase →
// MockOutbound → Close) against a recording transport using the checked-in
// golden case fragment-happy-path.case.json.

func goldenCasePathDogfood(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "spec", "examples", "cases")
	return filepath.Join(root, "fragment-happy-path.case.json")
}

func TestParityDogfoodFullFacade(t *testing.T) {
	transport := &recordingTransport{sessionID: "dogfood-session"}
	sp := New(Options{BaseURL: "http://fake-runtime.test", Transport: transport})

	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.ID() != "dogfood-session" {
		t.Fatalf("session.ID() = %q, want dogfood-session", session.ID())
	}

	if err := session.LoadCaseFromFile(goldenCasePathDogfood(t)); err != nil {
		t.Fatalf("LoadCaseFromFile: %v", err)
	}

	hit, err := session.FindInCase(CaseSpanPredicate{
		Direction: "outbound", Method: "GET", Path: "/fragment",
	})
	if err != nil {
		t.Fatalf("FindInCase: %v", err)
	}
	if hit == nil {
		t.Fatal("FindInCase returned nil hit")
	}
	if hit.Response.Status == 0 {
		t.Fatal("hit.Response.Status must be non-zero")
	}

	if err := session.MockOutbound(MockRuleSpec{
		ID:        "fragment-replay",
		Direction: "outbound",
		Method:    "GET",
		Path:      "/fragment",
		Response:  hit.Response,
	}); err != nil {
		t.Fatalf("MockOutbound: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify call sequence: create, load-case, rules, close.
	paths := make([]string, len(transport.calls))
	for i, c := range transport.calls {
		paths[i] = c.path
	}
	assertContainsSuffix(t, paths, "/v1/sessions")
	assertContainsSuffix(t, paths, "/load-case")
	assertContainsSuffix(t, paths, "/rules")
	assertContainsSuffix(t, paths, "/close")

	// rules body must reference /fragment
	for _, c := range transport.calls {
		if strings.HasSuffix(c.path, "/rules") && c.method == http.MethodPost {
			if !strings.Contains(c.body, "/fragment") {
				t.Fatalf("rules body does not mention /fragment: %q", c.body)
			}
			return
		}
	}
	t.Fatal("no POST /rules call found")
}

func assertContainsSuffix(t *testing.T, paths []string, suffix string) {
	t.Helper()
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			return
		}
	}
	t.Errorf("no call with path suffix %q; calls = %v", suffix, paths)
}
