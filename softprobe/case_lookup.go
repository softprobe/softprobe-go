package softprobe

import (
	"fmt"
	"strings"
)

// CaseSpanPredicate constrains which captured inject/extract spans
// FindInCase should match. All fields are optional; the zero value matches
// every inject/extract span. Mirrors the other SDKs' predicate types
// (see docs/design.md §3.2).
type CaseSpanPredicate struct {
	Direction  string // "inbound" | "outbound"
	Service    string
	Host       string
	HostSuffix string
	Method     string
	Path       string
	PathPrefix string
}

// CapturedResponse is the materialized HTTP response extracted from a
// captured span. Headers and Body are mutable; test authors are expected
// to mutate them (timestamps, tokens, …) before registering the result as
// a mock rule. Body is the raw string as it was captured.
type CapturedResponse struct {
	Status  int
	Headers map[string]string
	Body    string
}

// CapturedHit is the result of a successful FindInCase: the materialized
// response plus the raw OTLP span for advanced assertions.
type CapturedHit struct {
	Response CapturedResponse
	Span     map[string]any
}

const httpResponseHeaderPrefix = "http.response.header."

// FindSpans walks caseDocument (the parsed OTLP-shaped JSON) and returns
// every inject/extract span whose attributes satisfy predicate, in document
// order. Callers are the ergonomic SoftprobeSession.FindInCase wrapper.
func FindSpans(caseDocument map[string]any, predicate CaseSpanPredicate) []map[string]any {
	var matches []map[string]any
	if caseDocument == nil {
		return matches
	}
	traces, _ := caseDocument["traces"].([]any)
	for _, rawTrace := range traces {
		trace, _ := rawTrace.(map[string]any)
		resourceSpans, _ := trace["resourceSpans"].([]any)
		for _, rawRS := range resourceSpans {
			rs, _ := rawRS.(map[string]any)
			resource, _ := rs["resource"].(map[string]any)
			resourceAttrs := attributesFrom(resource)
			serviceName := readAttributeString(resourceAttrs, "service.name")
			scopeSpans, _ := rs["scopeSpans"].([]any)
			for _, rawSS := range scopeSpans {
				ss, _ := rawSS.(map[string]any)
				spans, _ := ss["spans"].([]any)
				for _, rawSpan := range spans {
					span, _ := rawSpan.(map[string]any)
					if span == nil {
						continue
					}
					if spanSatisfies(span, serviceName, predicate) {
						matches = append(matches, span)
					}
				}
			}
		}
	}
	return matches
}

// ResponseFromSpan materializes a CapturedResponse from an OTLP span's
// attributes. Returns an error if http.response.status_code is missing —
// captured spans without a status are authoring errors and must fail loudly.
func ResponseFromSpan(span map[string]any) (CapturedResponse, error) {
	attrs := attributesFrom(span)
	headers := map[string]string{}
	var status int
	var statusSet bool
	body := ""

	for _, rawAttr := range attrs {
		attr, _ := rawAttr.(map[string]any)
		if attr == nil {
			continue
		}
		key, _ := attr["key"].(string)
		value, _ := attr["value"].(map[string]any)
		switch {
		case key == "http.response.status_code":
			if n, ok := readIntValue(value); ok {
				status = n
				statusSet = true
			}
		case key == "http.response.body":
			body = anyValueToString(value)
		case strings.HasPrefix(key, httpResponseHeaderPrefix):
			name := key[len(httpResponseHeaderPrefix):]
			headers[name] = anyValueToString(value)
		}
	}

	if !statusSet {
		spanID, _ := span["spanId"].(string)
		if spanID == "" {
			spanID = "<unknown>"
		}
		return CapturedResponse{}, fmt.Errorf(
			"captured span %s is missing http.response.status_code; cannot materialize a captured response",
			spanID,
		)
	}

	return CapturedResponse{Status: status, Headers: headers, Body: body}, nil
}

// FormatPredicate produces a compact { key: "value", ... } string for
// error messages. Kept stable for cross-SDK log grepping.
func FormatPredicate(p CaseSpanPredicate) string {
	fields := []struct {
		key, value string
	}{
		{"direction", p.Direction},
		{"service", p.Service},
		{"host", p.Host},
		{"hostSuffix", p.HostSuffix},
		{"method", p.Method},
		{"path", p.Path},
		{"pathPrefix", p.PathPrefix},
	}
	var parts []string
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %q", f.key, f.value))
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func spanSatisfies(span map[string]any, resourceServiceName string, predicate CaseSpanPredicate) bool {
	attrs := attributesFrom(span)
	spanType := readAttributeString(attrs, "sp.span.type")
	if spanType != "inject" && spanType != "extract" {
		return false
	}

	if predicate.Direction != "" {
		if readAttributeString(attrs, "sp.traffic.direction") != predicate.Direction {
			return false
		}
	}

	if predicate.Method != "" {
		method := readAttributeString(attrs, "http.request.method")
		if method == "" {
			method = readAttributeString(attrs, "http.request.header.:method")
		}
		if method != predicate.Method {
			return false
		}
	}

	urlPath := readAttributeString(attrs, "url.path")
	if urlPath == "" {
		urlPath = readAttributeString(attrs, "http.request.header.:path")
	}
	if predicate.Path != "" && urlPath != predicate.Path {
		return false
	}
	if predicate.PathPrefix != "" && !strings.HasPrefix(urlPath, predicate.PathPrefix) {
		return false
	}

	host := readAttributeString(attrs, "url.host")
	if predicate.Host != "" && host != predicate.Host {
		return false
	}
	if predicate.HostSuffix != "" && !strings.HasSuffix(host, predicate.HostSuffix) {
		return false
	}

	service := readAttributeString(attrs, "sp.service.name")
	if service == "" {
		service = resourceServiceName
	}
	if predicate.Service != "" && service != predicate.Service {
		return false
	}

	return true
}

func attributesFrom(obj map[string]any) []any {
	if obj == nil {
		return nil
	}
	attrs, _ := obj["attributes"].([]any)
	return attrs
}

func readAttributeString(attrs []any, key string) string {
	for _, raw := range attrs {
		attr, _ := raw.(map[string]any)
		if attr == nil {
			continue
		}
		if k, _ := attr["key"].(string); k == key {
			value, _ := attr["value"].(map[string]any)
			return anyValueToString(value)
		}
	}
	return ""
}

func readIntValue(value map[string]any) (int, bool) {
	if value == nil {
		return 0, false
	}
	raw, ok := value["intValue"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func anyValueToString(value map[string]any) string {
	if value == nil {
		return ""
	}
	if s, ok := value["stringValue"].(string); ok {
		return s
	}
	if raw, ok := value["intValue"]; ok {
		switch v := raw.(type) {
		case float64:
			return fmt.Sprintf("%d", int64(v))
		case int:
			return fmt.Sprintf("%d", v)
		case int64:
			return fmt.Sprintf("%d", v)
		case string:
			return v
		}
	}
	if b, ok := value["boolValue"].(bool); ok {
		return fmt.Sprintf("%t", b)
	}
	if d, ok := value["doubleValue"].(float64); ok {
		return fmt.Sprintf("%g", d)
	}
	return ""
}
