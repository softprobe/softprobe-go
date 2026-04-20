// Package softprobe is the Go SDK for the Softprobe control runtime.
//
// It mirrors softprobe-js, softprobe-python, and softprobe-java: a thin
// HTTP client (Client) plus an ergonomic session facade (Softprobe /
// SoftprobeSession) that exposes LoadCaseFromFile, FindInCase, MockOutbound,
// ClearRules, SetPolicy, and Close.
//
// See docs/design.md §3.2 for the authoring flow.
package softprobe
