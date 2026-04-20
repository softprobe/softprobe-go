package softprobe

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultBaseURL is used when no base URL is configured and the
// SOFTPROBE_RUNTIME_URL env var is unset.
const DefaultBaseURL = "http://127.0.0.1:8080"

// Options configures a Softprobe facade.
type Options struct {
	BaseURL   string
	Transport Transport
}

// Softprobe is the ergonomic SDK facade, mirroring the TypeScript,
// Python, and Java counterparts. See docs/design.md §3.2.
type Softprobe struct {
	client *Client
}

// New constructs a Softprobe with the given options.
// An unset Options.BaseURL falls back to $SOFTPROBE_RUNTIME_URL or
// DefaultBaseURL, in that order.
func New(opts Options) *Softprobe {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv("SOFTPROBE_RUNTIME_URL")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	var clientOpts []ClientOption
	if opts.Transport != nil {
		clientOpts = append(clientOpts, WithTransport(opts.Transport))
	}
	return &Softprobe{client: NewClient(baseURL, clientOpts...)}
}

// StartSession creates a new session and returns a SoftprobeSession
// bound to it.
func (s *Softprobe) StartSession(mode string) (*SoftprobeSession, error) {
	resp, err := s.client.CreateSession(mode)
	if err != nil {
		return nil, err
	}
	if resp.SessionID == "" {
		return nil, fmt.Errorf("softprobe: runtime did not return sessionId in create-session response")
	}
	return &SoftprobeSession{id: resp.SessionID, client: s.client}, nil
}

// Attach binds an existing session id without any HTTP round-trip. Use this
// across processes or after a prior CLI `softprobe session start`.
func (s *Softprobe) Attach(sessionID string) *SoftprobeSession {
	return &SoftprobeSession{id: sessionID, client: s.client}
}

// MockRuleSpec describes a single outbound mock rule. Predicate fields are
// optional; Response is required. Mirrors SoftprobeMockRuleSpec in the
// other SDKs.
type MockRuleSpec struct {
	ID         string
	Priority   *int
	Direction  string
	Service    string
	Host       string
	HostSuffix string
	Method     string
	Path       string
	PathPrefix string
	Response   CapturedResponse
}

// SoftprobeSession is the session-bound helper. It holds the parsed case
// in memory after LoadCaseFromFile so FindInCase is a pure, synchronous
// lookup. It also accumulates MockOutbound rules locally so consecutive
// calls append to the rules document that is replaced on each POST
// (matching the ApplyRules semantics documented in docs/design.md §3.2.1).
type SoftprobeSession struct {
	id         string
	client     *Client
	rules      []map[string]any
	loadedCase map[string]any
}

// ID returns the session id.
func (s *SoftprobeSession) ID() string { return s.id }

// LoadCaseFromFile reads an OTLP-shaped case document from path, pushes it
// to the runtime, and keeps a parsed copy in memory for FindInCase.
func (s *SoftprobeSession) LoadCaseFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("softprobe: read case file: %w", err)
	}
	var caseDoc map[string]any
	if err := json.Unmarshal(data, &caseDoc); err != nil {
		return fmt.Errorf("softprobe: parse case file: %w", err)
	}
	s.loadedCase = caseDoc

	if _, err := s.client.LoadCase(s.id, data); err != nil {
		return err
	}
	return nil
}

// FindInCase performs a pure, in-memory lookup against the case most
// recently loaded via LoadCaseFromFile. It returns an error if zero or
// more than one span match, so authors disambiguate at authoring time.
func (s *SoftprobeSession) FindInCase(predicate CaseSpanPredicate) (*CapturedHit, error) {
	if s.loadedCase == nil {
		return nil, fmt.Errorf("softprobe.FindInCase: no case loaded; call LoadCaseFromFile first")
	}
	matches := FindSpans(s.loadedCase, predicate)
	if len(matches) == 0 {
		return nil, fmt.Errorf(
			"softprobe.FindInCase: no span in the loaded case matches %s; check the predicate or re-capture the case",
			FormatPredicate(predicate),
		)
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, span := range matches {
			id, _ := span["spanId"].(string)
			if id == "" {
				id = "<unknown>"
			}
			ids = append(ids, id)
		}
		return nil, fmt.Errorf(
			"softprobe.FindInCase: %d spans match %s; disambiguate the predicate — candidate span ids: %v",
			len(matches), FormatPredicate(predicate), ids,
		)
	}
	span := matches[0]
	resp, err := ResponseFromSpan(span)
	if err != nil {
		return nil, err
	}
	return &CapturedHit{Response: resp, Span: span}, nil
}

// MockOutbound appends a mock rule for the session and pushes the
// full rule-set to the runtime.
func (s *SoftprobeSession) MockOutbound(spec MockRuleSpec) error {
	s.rules = append(s.rules, buildMockRule(spec))
	return s.syncRules()
}

// ClearRules drops every rule registered in this session (both locally
// and on the runtime).
func (s *SoftprobeSession) ClearRules() error {
	s.rules = nil
	return s.syncRules()
}

// Close ends the session.
func (s *SoftprobeSession) Close() error {
	_, err := s.client.CloseSession(s.id)
	return err
}

func (s *SoftprobeSession) syncRules() error {
	payload := map[string]any{
		"version": 1,
		"rules":   s.rules,
	}
	// Emit an empty array (not null) when there are no rules so the wire
	// shape stays identical to the other SDKs and matches the schema.
	if s.rules == nil {
		payload["rules"] = []map[string]any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("softprobe: serialize rules: %w", err)
	}
	_, err = s.client.UpdateRules(s.id, body)
	return err
}

func buildMockRule(spec MockRuleSpec) map[string]any {
	when := map[string]any{}
	if spec.Direction != "" {
		when["direction"] = spec.Direction
	}
	if spec.Service != "" {
		when["service"] = spec.Service
	}
	switch {
	case spec.Host != "":
		when["host"] = spec.Host
	case spec.HostSuffix != "":
		when["host"] = spec.HostSuffix
	}
	if spec.Method != "" {
		when["method"] = spec.Method
	}
	if spec.Path != "" {
		when["path"] = spec.Path
	}
	if spec.PathPrefix != "" {
		when["pathPrefix"] = spec.PathPrefix
	}

	response := map[string]any{
		"status":  spec.Response.Status,
		"headers": ensureHeaders(spec.Response.Headers),
		"body":    spec.Response.Body,
	}

	rule := map[string]any{
		"when": when,
		"then": map[string]any{
			"action":   "mock",
			"response": response,
		},
	}

	if spec.ID != "" {
		rule["id"] = spec.ID
	}
	if spec.Priority != nil {
		rule["priority"] = *spec.Priority
	}

	return rule
}

func ensureHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}
