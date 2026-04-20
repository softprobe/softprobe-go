package softprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Transport is the minimal HTTP seam the Client uses. It lets tests inject
// an in-process fake without standing up a real HTTP server.
type Transport interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the thin control-plane client. It speaks only to /v1/sessions,
// /load-case, /rules, and /close; proxy OTLP lives elsewhere.
type Client struct {
	baseURL   string
	transport Transport
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithTransport injects a custom Transport (e.g. *http.Client or a test fake).
func WithTransport(t Transport) ClientOption {
	return func(c *Client) { c.transport = t }
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

	resp, err := c.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &RuntimeError{StatusCode: resp.StatusCode, Body: string(respBody)}
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
