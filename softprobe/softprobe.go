package softprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DefaultBaseURL is used when no base URL is configured and the
// SOFTPROBE_RUNTIME_URL env var is unset.
const DefaultBaseURL = "https://runtime.softprobe.dev"

// CaseLoadError is returned when a case document cannot be loaded (file read
// failure, JSON parse failure, or a non-typed runtime failure while pushing
// the case). Runtime-unreachable and unknown-session failures pass through
// with their typed form (*UnreachableError, *UnknownSessionError) so callers
// can distinguish them via errors.As.
type CaseLoadError struct {
	Message string
	Cause   error
}

func (e *CaseLoadError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *CaseLoadError) Unwrap() error { return e.Cause }

// CaseLookupAmbiguityError is returned by FindInCase when more than one span
// matches the predicate. Authors disambiguate the predicate at authoring time.
type CaseLookupAmbiguityError struct {
	Count   int
	Matches []string
	Message string
}

func (e *CaseLookupAmbiguityError) Error() string { return e.Message }

// Options configures a Softprobe facade.
type Options struct {
	BaseURL   string
	Transport Transport
	// APIToken is sent as `Authorization: Bearer <APIToken>` on every runtime
	// call. When empty, falls back to the SOFTPROBE_API_TOKEN environment
	// variable. Whitespace-only values are treated as "no token" — matching
	// the runtime's withOptionalBearerAuth contract.
	APIToken string
	// APITokenSet lets callers pass an empty APIToken while still overriding
	// the SOFTPROBE_API_TOKEN env var. It's useful in tests that want to
	// explicitly disable auth headers.
	APITokenSet bool
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
	if opts.APIToken != "" || opts.APITokenSet {
		clientOpts = append(clientOpts, WithAPIToken(opts.APIToken))
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
// to the runtime, and keeps a parsed copy in memory for FindInCase. File and
// parse failures are wrapped in *CaseLoadError; runtime failures pass through
// (*UnreachableError / *UnknownSessionError) and are otherwise wrapped in
// *CaseLoadError too.
func (s *SoftprobeSession) LoadCaseFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &CaseLoadError{Message: "softprobe: read case file", Cause: err}
	}
	return s.LoadCase(data)
}

// LoadCase pushes an already-prepared JSON case document to the runtime and
// keeps a parsed copy in memory for FindInCase. Parse failures are wrapped
// in *CaseLoadError; runtime failures pass through (*UnreachableError /
// *UnknownSessionError) and are otherwise wrapped in *CaseLoadError too.
func (s *SoftprobeSession) LoadCase(caseJSON []byte) error {
	var caseDoc map[string]any
	if err := json.Unmarshal(caseJSON, &caseDoc); err != nil {
		return &CaseLoadError{Message: "softprobe: parse case document", Cause: err}
	}
	if _, err := s.client.LoadCase(s.id, caseJSON); err != nil {
		var ue *UnreachableError
		var use *UnknownSessionError
		if errors.As(err, &ue) || errors.As(err, &use) {
			return err
		}
		return &CaseLoadError{Message: "softprobe: load case into runtime", Cause: err}
	}
	s.loadedCase = caseDoc
	return nil
}

// FindInCase performs a pure, in-memory lookup against the case most
// recently loaded via LoadCaseFromFile. It returns an error if zero spans
// match, and a *CaseLookupAmbiguityError when more than one span matches so
// authors disambiguate at authoring time.
func (s *SoftprobeSession) FindInCase(predicate CaseSpanPredicate) (*CapturedHit, error) {
	matches, err := s.lookup(predicate)
	if err != nil {
		return nil, err
	}
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
		return nil, &CaseLookupAmbiguityError{
			Count:   len(matches),
			Matches: ids,
			Message: fmt.Sprintf(
				"softprobe.FindInCase: %d spans match %s; disambiguate the predicate — candidate span ids: %v",
				len(matches), FormatPredicate(predicate), ids,
			),
		}
	}
	span := matches[0]
	resp, err := ResponseFromSpan(span)
	if err != nil {
		return nil, err
	}
	return &CapturedHit{Response: resp, Span: span}, nil
}

// FindAllInCase returns every span that matches the predicate. Never errors
// on zero matches — callers handle the empty slice. Use FindInCase when you
// expect exactly one match and want authoring-time errors for ambiguity.
func (s *SoftprobeSession) FindAllInCase(predicate CaseSpanPredicate) ([]CapturedHit, error) {
	matches, err := s.lookup(predicate)
	if err != nil {
		return nil, err
	}
	hits := make([]CapturedHit, 0, len(matches))
	for _, span := range matches {
		resp, err := ResponseFromSpan(span)
		if err != nil {
			return nil, err
		}
		hits = append(hits, CapturedHit{Response: resp, Span: span})
	}
	return hits, nil
}

func (s *SoftprobeSession) lookup(predicate CaseSpanPredicate) ([]map[string]any, error) {
	if s.loadedCase == nil {
		return nil, &CaseLoadError{
			Message: "softprobe.FindInCase: no case loaded; call LoadCaseFromFile first",
		}
	}
	return FindSpans(s.loadedCase, predicate), nil
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

// SetPolicy replaces the session's external HTTP policy (e.g. strict vs allow).
func (s *SoftprobeSession) SetPolicy(policyJSON []byte) error {
	_, err := s.client.SetPolicy(s.id, policyJSON)
	return err
}

// SetAuthFixtures pushes an auth fixtures document to
// POST /v1/sessions/{id}/fixtures/auth.
func (s *SoftprobeSession) SetAuthFixtures(fixturesJSON []byte) error {
	_, err := s.client.SetAuthFixtures(s.id, fixturesJSON)
	return err
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
