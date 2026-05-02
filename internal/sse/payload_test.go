package sse

import (
	"bytes"
	"testing"
)

func TestExtractSSEURL(t *testing.T) {
	cases := []struct {
		name              string
		payload           string
		wantURL           string
		wantInactivitySec int
		wantOK            bool
	}{
		{
			name:              "url form",
			payload:           `{"eventStreams":[{"type":"sse","url":"https://sse.example.com/stream?channels=abc","inactivityDelaySec":120}]}`,
			wantURL:           "https://sse.example.com/stream?channels=abc",
			wantInactivitySec: 120,
			wantOK:            true,
		},
		{
			name:              "first sse entry wins",
			payload:           `{"eventStreams":[{"type":"other","url":"https://other.example.com"},{"type":"sse","url":"https://sse.example.com/stream","inactivityDelaySec":60}]}`,
			wantURL:           "https://sse.example.com/stream",
			wantInactivitySec: 60,
			wantOK:            true,
		},
		{
			name:    "no sse type",
			payload: `{"eventStreams":[{"type":"grpc","url":"https://grpc.example.com"}]}`,
			wantOK:  false,
		},
		{
			name:    "empty eventStreams array",
			payload: `{"eventStreams":[]}`,
			wantOK:  false,
		},
		{
			name:    "missing eventStreams key",
			payload: `{"flags":{}}`,
			wantOK:  false,
		},
		{
			name:    "malformed JSON",
			payload: `not json`,
			wantOK:  false,
		},
		{
			name:    "empty payload",
			payload: ``,
			wantOK:  false,
		},
		{
			name:    "sse type with empty url",
			payload: `{"eventStreams":[{"type":"sse","url":""}]}`,
			wantOK:  false,
		},
		{
			name:              "zero inactivity delay",
			payload:           `{"eventStreams":[{"type":"sse","url":"https://sse.example.com/stream"}]}`,
			wantURL:           "https://sse.example.com/stream",
			wantInactivitySec: 0,
			wantOK:            true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotSec, gotOK := ExtractSSEURL([]byte(tc.payload))
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotURL != tc.wantURL {
				t.Errorf("url = %q, want %q", gotURL, tc.wantURL)
			}
			if gotSec != tc.wantInactivitySec {
				t.Errorf("inactivityDelaySec = %d, want %d", gotSec, tc.wantInactivitySec)
			}
		})
	}
}

func TestRewriteSSEURL(t *testing.T) {
	const upstream = "https://upstream.sse.example.com/stream?ch=abc"
	const proxy = "http://proxy.example.com/sse?sdk_key=dt_server_us_abc123"

	cases := []struct {
		name       string
		payload    string
		proxyURL   string
		wantResult RewriteResult
		// wantContains is checked in the output if wantResult == RewriteResultRewritten.
		wantContains string
		// wantByteIdentical asserts the returned bytes equal the input payload.
		wantByteIdentical bool
	}{
		{
			name:         "rewrites upstream URL",
			payload:      `{"flags":{},"eventStreams":[{"type":"sse","url":"` + upstream + `","inactivityDelaySec":120}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: proxy,
		},
		{
			name:              "no SSE entry returns payload unchanged",
			payload:           `{"flags":{},"eventStreams":[{"type":"grpc","url":"https://grpc.example.com"}]}`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
		},
		{
			name:              "empty eventStreams returns payload unchanged",
			payload:           `{"flags":{},"eventStreams":[]}`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
		},
		{
			name:              "malformed JSON returns payload unchanged",
			payload:           `not json`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
		},
		{
			name:              "missing eventStreams returns payload unchanged",
			payload:           `{"flags":{}}`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
		},
		{
			name: "upstream URL not in body bytes returns payload unchanged",
			// ExtractSSEURL succeeds, but the URL was encoded differently so the
			// byte form doesn't appear in the raw payload. This exercises NotFoundInBody.
			// We simulate this by having the URL appear with escaped forward slashes (\/),
			// which json.Marshal would not produce, so oldBytes won't match.
			payload:           `{"eventStreams":[{"type":"sse","url":"https:\/\/upstream.sse.example.com\/stream","inactivityDelaySec":0}]}`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNotFoundInBody,
			wantByteIdentical: true,
		},
		{
			name:         "surrounding bytes preserved",
			payload:      `{"before":"value","eventStreams":[{"type":"sse","url":"` + upstream + `"}],"after":"value"}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `"before":"value"`,
		},
		{
			name:         "upstream URL absent from rewritten output",
			payload:      `{"flags":{},"eventStreams":[{"type":"sse","url":"` + upstream + `","inactivityDelaySec":60}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.payload)
			out, result := RewriteSSEURL(input, tc.proxyURL)

			if result != tc.wantResult {
				t.Fatalf("result = %v, want %v", result, tc.wantResult)
			}

			if tc.wantByteIdentical && !bytes.Equal(out, input) {
				t.Errorf("expected byte-identical output, got diff:\nout:   %q\ninput: %q", out, input)
			}

			if tc.wantContains != "" && !bytes.Contains(out, []byte(tc.wantContains)) {
				t.Errorf("output missing %q, got: %q", tc.wantContains, out)
			}

			// For a successful rewrite, the upstream URL must no longer appear.
			if result == RewriteResultRewritten && bytes.Contains(out, []byte(upstream)) {
				t.Errorf("upstream URL still present in rewritten output: %q", out)
			}
		})
	}
}
