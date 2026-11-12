package sse

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

func TestExtractSSEURL(t *testing.T) {
	cases := []struct {
		name              string
		payload           string
		fallbackOrigin    string
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
			name:              "endpoint form",
			payload:           `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com","requestUri":"/event-stream?channels=abc&key=t"},"inactivityDelaySec":90}]}`,
			wantURL:           "https://sse.example.com/event-stream?channels=abc&key=t",
			wantInactivitySec: 90,
			wantOK:            true,
		},
		{
			name:    "endpoint preferred over url",
			payload: `{"eventStreams":[{"type":"sse","url":"https://old.example.com/stream","endpoint":{"origin":"https://sse.example.com","requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com/event-stream",
			wantOK:  true,
		},
		{
			name:    "endpoint origin with trailing slash",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com/","requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com/event-stream",
			wantOK:  true,
		},
		{
			name:    "endpoint origin with port",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com:8443","requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com:8443/event-stream",
			wantOK:  true,
		},
		{
			name:           "endpoint without origin uses fallback origin",
			payload:        `{"eventStreams":[{"type":"sse","endpoint":{"requestUri":"/event-stream?c=1"}}]}`,
			fallbackOrigin: "https://cdn.us.fm.example.com",
			wantURL:        "https://cdn.us.fm.example.com/event-stream?c=1",
			wantOK:         true,
		},
		{
			name:    "endpoint without origin and no fallback",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"requestUri":"/event-stream"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint without origin falls back to url",
			payload: `{"eventStreams":[{"type":"sse","url":"https://sse.example.com/stream","endpoint":{"requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com/stream",
			wantOK:  true,
		},
		{
			name:    "invalid endpoint falls back to url",
			payload: `{"eventStreams":[{"type":"sse","url":"https://sse.example.com/stream","endpoint":{"origin":"http://sse.example.com","requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com/stream",
			wantOK:  true,
		},
		{
			name:    "endpoint with non-https origin",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"http://sse.example.com","requestUri":"/event-stream"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint origin with path",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com/base","requestUri":"/event-stream"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint origin with userinfo",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://user@sse.example.com","requestUri":"/event-stream"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint requestUri without leading slash cannot change host",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com","requestUri":"@internal.local/x"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint requestUri network-path reference rejected",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com","requestUri":"//internal.local/x"}}]}`,
			wantOK:  false,
		},
		{
			name:    "endpoint with empty requestUri",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://sse.example.com","requestUri":""}}]}`,
			wantOK:  false,
		},
		{
			name:    "unusable sse entry skipped for next sse entry",
			payload: `{"eventStreams":[{"type":"sse","endpoint":{"origin":"http://bad.example.com","requestUri":"/x"}},{"type":"sse","endpoint":{"origin":"https://sse.example.com","requestUri":"/event-stream"}}]}`,
			wantURL: "https://sse.example.com/event-stream",
			wantOK:  true,
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
			gotURL, gotSec, gotOK := ExtractSSEURL([]byte(tc.payload), tc.fallbackOrigin)
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

func TestOriginOf(t *testing.T) {
	cases := map[string]string{
		"https://cdn.us.fm.example.com/server/dt01.key.json": "https://cdn.us.fm.example.com",
		"https://cdn.example.com:8443/x?y=z":                 "https://cdn.example.com:8443",
		"":                                                   "",
		"/relative/path":                                     "",
	}
	for in, want := range cases {
		if got := OriginOf(in); got != want {
			t.Errorf("OriginOf(%q) = %q, want %q", in, got, want)
		}
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
			name:            "url form stays url",
			payload:         `{"flags":{},"eventStreams":[{"type":"sse","url":"` + upstream + `","inactivityDelaySec":120}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    `"url":"` + proxy + `"`,
			wantNotContains: `"endpoint"`,
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
			name:            "endpoint form rewritten to proxy endpoint",
			payload:         `{"flags":{},"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream?ch=abc"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    `"endpoint":{"origin":"http://proxy.example.com","requestUri":"/sse?stream_id=dt01.server_us_ab3xy9kp2q"}`,
			wantNotContains: "upstream.sse.example.com",
		},
		{
			name:            "endpoint form does not gain a url field",
			payload:         `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: `"url"`,
		},
		{
			name:            "both forms rewritten",
			payload:         `{"eventStreams":[{"type":"sse","url":"` + upstream + `","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    `"url":"` + proxy + `"`,
			wantNotContains: "upstream.sse.example.com",
		},
		{
			name:            "endpoint without origin rewritten",
			payload:         `{"eventStreams":[{"type":"sse","endpoint":{"requestUri":"/event-stream?ch=abc"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantContains:    `"origin":"http://proxy.example.com"`,
			wantNotContains: "/event-stream",
		},
		{
			name:         "unknown entry fields preserved",
			payload:      `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"},"futureField":{"a":1}}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `"futureField":{"a":1}`,
		},
		{
			name:            "absent inactivityDelaySec not added",
			payload:         `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: "inactivityDelaySec",
		},
		{
			name:         "inactivityDelaySec kept",
			payload:      `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"},"inactivityDelaySec":45}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `"inactivityDelaySec":45`,
		},
		{
			name:            "endpoint with invalid requestUri rewritten",
			payload:         `{"eventStreams":[{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"event-stream"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: "upstream.sse.example.com",
		},
		{
			// The proxy connects to entry 1, but an SDK would take entry 0.
			name:            "rejected entry before usable entry both rewritten",
			payload:         `{"eventStreams":[{"type":"sse","endpoint":{"origin":"http://bad.example.com","requestUri":"/x"}},{"type":"sse","endpoint":{"origin":"https://upstream.sse.example.com","requestUri":"/event-stream"}}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: "bad.example.com",
		},
		{
			name:            "every usable entry rewritten",
			payload:         `{"eventStreams":[{"type":"sse","url":"` + upstream + `"},{"type":"sse","url":"https://other.sse.example.com/stream"}]}`,
			proxyURL:        proxy,
			wantResult:      RewriteResultRewritten,
			wantNotContains: "sse.example.com",
		},
		{
			name:         "non-sse entries untouched",
			payload:      `{"eventStreams":[{"type":"grpc","url":"https://grpc.example.com"},{"type":"sse","url":"` + upstream + `"}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `"url":"https://grpc.example.com"`,
		},
		{
			name:         "sse entry without endpoint or url left alone",
			payload:      `{"eventStreams":[{"type":"sse","inactivityDelaySec":30},{"type":"sse","url":"` + upstream + `"}]}`,
			proxyURL:     proxy,
			wantResult:   RewriteResultRewritten,
			wantContains: `{"inactivityDelaySec":30,"type":"sse"}`,
		},
		{
			name:              "only sse entries without endpoint or url returns payload unchanged",
			payload:           `{"eventStreams":[{"type":"sse","inactivityDelaySec":30}]}`,
			proxyURL:          proxy,
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
		},
		{
			name:              "relative proxy URL leaves payload unchanged",
			payload:           `{"eventStreams":[{"type":"sse","url":"` + upstream + `"}]}`,
			proxyURL:          "/sse?stream_id=x",
			wantResult:        RewriteResultNoEntry,
			wantByteIdentical: true,
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
			if result == RewriteResultRewritten {
				// Every rewrite must leave origin + requestUri equal to the proxy URL.
				// The rewritten entry keeps the input's forms, each pointing at the proxy.
				checkRewrittenForms(t, input, out, tc.proxyURL)
			}

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

// checkRewrittenForms asserts that every type=="sse" entry in out kept the
// forms (endpoint, url) it had in in, each pointing at proxyURL, and that
// other entries are unchanged.
func checkRewrittenForms(t *testing.T, in, out []byte, proxyURL string) {
	t.Helper()
	streams := func(payload []byte) []streamEntry {
		var cfg struct {
			EventStreams []streamEntry `json:"eventStreams"`
		}
		if err := json.Unmarshal(payload, &cfg); err != nil {
			t.Fatalf("payload is not valid JSON: %v", err)
		}
		return cfg.EventStreams
	}
	ins, outs := streams(in), streams(out)
	if len(ins) != len(outs) {
		t.Fatalf("eventStreams length: input %d, output %d", len(ins), len(outs))
	}
	for i, before := range ins {
		after := outs[i]
		if before.Type != "sse" {
			if !reflect.DeepEqual(before, after) {
				t.Errorf("entry %d (type %q) changed: %+v -> %+v", i, before.Type, before, after)
			}
			continue
		}
		if (before.Endpoint != nil) != (after.Endpoint != nil) {
			t.Errorf("entry %d endpoint present: input %v, output %v", i, before.Endpoint != nil, after.Endpoint != nil)
		}
		if (before.URL != "") != (after.URL != "") {
			t.Errorf("entry %d url present: input %v, output %v", i, before.URL != "", after.URL != "")
		}
		if after.Endpoint != nil {
			if got := after.Endpoint.Origin + after.Endpoint.RequestURI; got != proxyURL {
				t.Errorf("entry %d endpoint resolves to %q, want %q", i, got, proxyURL)
			}
		}
		if after.URL != "" && after.URL != proxyURL {
			t.Errorf("entry %d url = %q, want %q", i, after.URL, proxyURL)
		}
	}
}
