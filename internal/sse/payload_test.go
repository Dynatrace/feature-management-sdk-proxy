package sse

import (
	"bytes"
	"testing"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
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
	proxy := "http://proxy.example.com/sse?stream_id=" + sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	cases := []struct {
		name       string
		payload    string
		proxyURL   string
		wantResult RewriteResult
		// wantContains is checked in the output if wantResult == RewriteResultRewritten.
		wantContains string
		// wantNotContains is checked in the output if wantResult == RewriteResultRewritten.
		wantNotContains string
		// wantByteIdentical asserts the returned bytes equal the input payload.
		wantByteIdentical bool
	}{
		{
			name:            "rewrites upstream URL",
			payload:         `{"flags":{},"eventStreams":[{"type":"sse","url":"` + upstream + `","inactivityDelaySec":120}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    proxy,
			wantNotContains: upstream,
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
			name: "escaped forward slashes in URL still rewritten",
			// JSON-encoded forward slashes (\/) are decoded during unmarshal,
			// so the URL is correctly matched and replaced.
			payload:         `{"eventStreams":[{"type":"sse","url":"https:\/\/upstream.sse.example.com\/stream","inactivityDelaySec":0}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    proxy,
			wantNotContains: "upstream.sse.example.com",
		},
		{
			name:         "surrounding fields preserved",
			payload:      `{"before":"value","eventStreams":[{"type":"sse","url":"` + upstream + `"}],"after":"value"}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `"before":"value"`,
		},
		{
			name:            "upstream URL absent from rewritten output",
			payload:         `{"flags":{},"eventStreams":[{"type":"sse","url":"` + upstream + `","inactivityDelaySec":60}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: upstream,
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

			if tc.wantNotContains != "" && bytes.Contains(out, []byte(tc.wantNotContains)) {
				t.Errorf("output should not contain %q, got: %q", tc.wantNotContains, out)
			}
		})
	}
}
