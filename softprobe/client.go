package softprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RuntimeError is the stable error type surfaced by Client for non-2xx
// responses from the control runtime. It mirrors SoftprobeRuntimeError in the
// TypeScript, Python, and Java SDKs.
type RuntimeError struct {
	StatusCode int
	Body       string
}

func (e *RuntimeError) Error() string {
	return fmt.Sprintf("softprobe runtime error: status=%d body=%s", e.StatusCode, e.Body)
}

// UnreachableError is returned when the transport layer fails before an HTTP
// response is available (connection refused, DNS failure, timeout, ...).
// Mirrors SoftprobeRuntimeUnreachableError in the other SDKs.
type UnreachableError struct{ Cause error }

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("softprobe runtime is unreachable: %v", e.Cause)
}

func (e *UnreachableError) Unwrap() error { return e.Cause }

// UnknownSessionError is returned when the runtime replies with the stable
// `{"error":{"code":"unknown_session", ...}}` envelope. It embeds RuntimeError
// so callers that only care about the HTTP status can still match via
// errors.As on *RuntimeError.
type UnknownSessionError struct{ RuntimeError }

func (e *UnknownSessionError) Error() string {
	return fmt.Sprintf("softprobe unknown session: status=%d body=%s", e.StatusCode, e.Body)
}

// As lets `errors.As(err, &*RuntimeError)` recover the underlying HTTP
// fields, which Go's default embedded-struct handling does not expose.
func (e *UnknownSessionError) As(target any) bool {
	if t, ok := target.(**RuntimeError); ok {
		*t = &e.RuntimeError
		return true
	}
	return false
}

// Transport is the minimal HTTP seam the Client uses. It lets tests inject
// an in-process fake without standing up a real HTTP server.
type Transport interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the thin control-plane client. It speaks only to /v1/sessions,
// /load-case, /rules, /policy, and /close; proxy OTLP lives elsewhere.
type Client struct {
	baseURL   string
	transport Transport
	// apiToken is sent as `Authorization: Bearer <token>` on every request when
	// non-empty. When this struct field is the zero string, we fall back to
	// the SOFTPROBE_API_TOKEN environment variable at request time so tests
	// using t.Setenv work without having to re-construct the client.
	apiToken apiTokenSource
}

type apiTokenSource struct {
	explicit string
	hasValue bool
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithTransport injects a custom Transport (e.g. *http.Client or a test fake).
func WithTransport(t Transport) ClientOption {
	return func(c *Client) { c.transport = t }
}

// WithAPIToken configures the bearer token sent on every runtime call. An
// empty string here still counts as "explicitly configured" and overrides the
// SOFTPROBE_API_TOKEN env var; pass the option only when the caller wants to
// set an explicit value.
func WithAPIToken(token string) ClientOption {
	return func(c *Client) { c.apiToken = apiTokenSource{explicit: token, hasValue: true} }
}

// NewClient constructs a Client bound to the given runtime URL.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL:   baseURL,
		transport: &http.Client{Timeout: 5 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SessionCreateResponse mirrors the JSON returned by POST /v1/sessions.
type SessionCreateResponse struct {
	SessionID       string `json:"sessionId"`
	SessionRevision int    `json:"sessionRevision,omitempty"`
	Closed          bool   `json:"closed,omitempty"`
}

// CreateSession posts to /v1/sessions.
func (c *Client) CreateSession(mode string) (*SessionCreateResponse, error) {
	body, err := json.Marshal(map[string]string{"mode": mode})
	if err != nil {
		return nil, err
	}
	return c.postJSON("/v1/sessions", body)
}

// LoadCase posts the given case document to /v1/sessions/{id}/load-case.
func (c *Client) LoadCase(sessionID string, caseJSON []byte) (*SessionCreateResponse, error) {
	return c.postJSON("/v1/sessions/"+sessionID+"/load-case", caseJSON)
}

// UpdateRules posts the given rules document to /v1/sessions/{id}/rules.
func (c *Client) UpdateRules(sessionID string, rulesJSON []byte) (*SessionCreateResponse, error) {
	return c.postJSON("/v1/sessions/"+sessionID+"/rules", rulesJSON)
}

// SetPolicy posts the given policy document to /v1/sessions/{id}/policy.
func (c *Client) SetPolicy(sessionID string, policyJSON []byte) (*SessionCreateResponse, error) {
	return c.postJSON("/v1/sessions/"+sessionID+"/policy", policyJSON)
}

// SetAuthFixtures posts the given fixtures document to
// /v1/sessions/{id}/fixtures/auth.
func (c *Client) SetAuthFixtures(sessionID string, fixturesJSON []byte) (*SessionCreateResponse, error) {
	return c.postJSON("/v1/sessions/"+sessionID+"/fixtures/auth", fixturesJSON)
}

// CloseSession posts to /v1/sessions/{id}/close.
func (c *Client) CloseSession(sessionID string) (*SessionCreateResponse, error) {
	return c.postJSON("/v1/sessions/"+sessionID+"/close", []byte("{}"))
}

func (c *Client) postJSON(path string, body []byte) (*SessionCreateResponse, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := c.resolveBearerToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.transport.Do(req)
	if err != nil {
		return nil, &UnreachableError{Cause: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyRuntimeError(resp.StatusCode, string(respBody))
	}

	if len(bytes.TrimSpace(respBody)) == 0 {
		return &SessionCreateResponse{}, nil
	}

	var out SessionCreateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode control response: %w", err)
	}
	return &out, nil
}

// resolveBearerToken returns the effective bearer token for this client.
// The explicit WithAPIToken option wins (even when empty — that's an
// intentional "disable env fallback" signal). Otherwise we read
// SOFTPROBE_API_TOKEN. Whitespace-only values collapse to the empty string.
func (c *Client) resolveBearerToken() string {
	var candidate string
	if c.apiToken.hasValue {
		candidate = c.apiToken.explicit
	} else {
		candidate = os.Getenv("SOFTPROBE_API_TOKEN")
	}
	return strings.TrimSpace(candidate)
}

// classifyRuntimeError distinguishes the stable unknown_session envelope from
// every other non-2xx response so callers can `errors.As` to the typed
// UnknownSessionError without parsing messages.
func classifyRuntimeError(status int, body string) error {
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if parsed.Error.Code == "unknown_session" {
			return &UnknownSessionError{RuntimeError: RuntimeError{StatusCode: status, Body: body}}
		}
	}
	return &RuntimeError{StatusCode: status, Body: body}
}
