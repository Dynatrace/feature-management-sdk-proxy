package sse

import "testing"

func TestExtractSSEURL(t *testing.T) {
	cases := []struct {
		name               string
		payload            string
		wantURL            string
		wantInactivitySec  int
		wantOK             bool
	}{
		{
			name:              "url form",
			payload:           `{"eventStreams":[{"type":"sse","url":"https://sse.example.com/stream?channels=abc","inactivityDelaySec":120}]}`,
			wantURL:           "https://sse.example.com/stream?channels=abc",
			wantInactivitySec: 120,
			wantOK:            true,
		},
		{
			name:    "first sse entry wins",
			payload: `{"eventStreams":[{"type":"other","url":"https://other.example.com"},{"type":"sse","url":"https://sse.example.com/stream","inactivityDelaySec":60}]}`,
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
