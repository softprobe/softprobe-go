package softprobe

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// headerCapturingTransport records the Authorization header attached to every
// outbound request so the auth tests can assert the SDK's bearer-token wiring
// against the runtime's withOptionalBearerAuth contract.
type headerCapturingTransport struct {
	auths []string
	paths []string
}

func (t *headerCapturingTransport) Do(req *http.Request) (*http.Response, error) {
	t.auths = append(t.auths, req.Header.Get("Authorization"))
	t.paths = append(t.paths, req.URL.Path)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{"sessionId":"s","sessionRevision":0}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func TestClientAttachesBearerFromExplicitAPITokenOption(t *testing.T) {
	tr := &headerCapturingTransport{}
	client := NewClient("http://runtime.test", WithTransport(tr), WithAPIToken("sp_explicit_token"))

	if _, err := client.CreateSession("replay"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if got, want := tr.auths[0], "Bearer sp_explicit_token"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestClientFallsBackToSOFTPROBEAPITokenEnvVar(t *testing.T) {
	t.Setenv("SOFTPROBE_API_TOKEN", "sp_env_token")
	tr := &headerCapturingTransport{}
	client := NewClient("http://runtime.test", WithTransport(tr))

	if _, err := client.CreateSession("replay"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if got, want := tr.auths[0], "Bearer sp_env_token"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestClientAPITokenOptionOverridesEnvVar(t *testing.T) {
	t.Setenv("SOFTPROBE_API_TOKEN", "sp_env_token")
	tr := &headerCapturingTransport{}
	client := NewClient("http://runtime.test", WithTransport(tr), WithAPIToken("sp_explicit_wins"))

	if _, err := client.CreateSession("replay"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if got, want := tr.auths[0], "Bearer sp_explicit_wins"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestClientSendsNoAuthorizationHeaderWhenUnconfigured(t *testing.T) {
	t.Setenv("SOFTPROBE_API_TOKEN", "")
	tr := &headerCapturingTransport{}
	client := NewClient("http://runtime.test", WithTransport(tr))

	if _, err := client.CreateSession("replay"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if tr.auths[0] != "" {
		t.Fatalf("Authorization = %q, want empty", tr.auths[0])
	}
}

func TestClientTreatsWhitespaceTokenAsNoToken(t *testing.T) {
	t.Setenv("SOFTPROBE_API_TOKEN", "   ")
	tr := &headerCapturingTransport{}
	client := NewClient("http://runtime.test", WithTransport(tr), WithAPIToken(""))

	if _, err := client.CreateSession("replay"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if tr.auths[0] != "" {
		t.Fatalf("Authorization = %q, want empty", tr.auths[0])
	}
}

func TestFacadeThreadsAPITokenThroughEveryCall(t *testing.T) {
	tr := &headerCapturingTransport{}
	sp := New(Options{BaseURL: "http://runtime.test", Transport: tr, APIToken: "sp_facade_token"})

	session, err := sp.StartSession("replay")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(tr.auths) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(tr.auths))
	}
	for i, auth := range tr.auths {
		if auth != "Bearer sp_facade_token" {
			t.Errorf("call %d (%s): Authorization = %q, want Bearer sp_facade_token", i, tr.paths[i], auth)
		}
	}
}
