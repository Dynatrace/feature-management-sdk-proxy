package sdkkey

import (
	"strings"
	"testing"
)

const (
	testFullKey  = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	testPublicID = "dt01.server_us_ab3xy9kp2q"
)

func TestPublicID_TwoSegmentKey(t *testing.T) {
	got := PublicID(testFullKey)
	if got != testPublicID {
		t.Errorf("PublicID = %q, want %q", got, testPublicID)
	}
}

func TestPublicID_NoDot_ReturnsEmpty(t *testing.T) {
	if got := PublicID("nodots"); got != "" {
		t.Errorf("PublicID(%q) = %q, want empty", "nodots", got)
	}
}

func TestPublicID_MatchesSplitRule(t *testing.T) {
	parts := strings.Split(testFullKey, ".")
	want := strings.Join(parts[:len(parts)-1], ".")
	got := PublicID(testFullKey)
	if got != want {
		t.Errorf("PublicID = %q, want %q", got, want)
	}
}

func TestIsValidID(t *testing.T) {
	valid := []string{
		"dt01.server_us_ab3xy9kp2q",
		"dt02.mobile_eu_0000000000",
		"dt99.web_ap_xXxXxXxXxX",
		"dt00.server_oc_aaaaaaaaaa",
	}
	for _, v := range valid {
		if !IsValidID(v) {
			t.Errorf("IsValidID(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"",
		"abc",
		"dt01.invalid_us_ab3xy9kp2q",
		"DT01.server_us_ab3xy9kp2q",
		"dt01.server_US_ab3xy9kp2q",
		"dt01.server_us_abc",
		"dt01.server_us_ab3xy9kp2q!",
		"dt01.server_us_ab3xy9kp2qq", // 11-char publicRandom
		testFullKey,                  // full key, not public ID
	}
	for _, v := range invalid {
		if IsValidID(v) {
			t.Errorf("IsValidID(%q) = true, want false", v)
		}
	}
}

func TestValidateFullKey(t *testing.T) {
	if !ValidateFullKey(testFullKey) {
		t.Errorf("ValidateFullKey(%q) = false, want true", testFullKey)
	}
	invalid := []string{
		"",
		testPublicID,                          // public only, missing private segment
		"dt01.server_us_ab3xy9kp2q.short_abc", // secret too short
		"dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f_d4f8a9c12a", // secret 31 hex
		"bogus",
	}
	for _, v := range invalid {
		if ValidateFullKey(v) {
			t.Errorf("ValidateFullKey(%q) = true, want false", v)
		}
	}
}

func TestExtractGeo(t *testing.T) {
	cases := []struct {
		sdkKey  string
		wantGeo string
		wantOK  bool
	}{
		{"dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "us", true},
		{"dt01.server_eu_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "eu", true},
		{"dt01.server_ap_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "ap", true},
		{"dt01.server_us_ab3xy9kp2q", "", false}, // public ID only
		{"bogus", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		geo, ok := ExtractGeo(tc.sdkKey)
		if ok != tc.wantOK || geo != tc.wantGeo {
			t.Errorf("ExtractGeo(%q) = (%q, %v), want (%q, %v)", tc.sdkKey, geo, ok, tc.wantGeo, tc.wantOK)
		}
	}
}

func TestFromFileName(t *testing.T) {
	cases := []struct {
		file    string
		wantKey string
		wantOK  bool
	}{
		{"abc123.json", "abc123", true},
		{"dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", true},
		{".json", "", false},
		{"noextension", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		key, ok := FromFileName(tc.file)
		if ok != tc.wantOK || key != tc.wantKey {
			t.Errorf("FromFileName(%q) = (%q, %v), want (%q, %v)", tc.file, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}

func TestParse_FullKey(t *testing.T) {
	k, ok := Parse(testFullKey)
	if !ok {
		t.Fatalf("Parse(%q) returned ok=false", testFullKey)
	}
	if k.Raw() != testFullKey {
		t.Errorf("Raw() = %q, want %q", k.Raw(), testFullKey)
	}
	if k.PublicID() != testPublicID {
		t.Errorf("PublicID() = %q, want %q", k.PublicID(), testPublicID)
	}
	if k.Token() != "dt01" {
		t.Errorf("Token() = %q, want %q", k.Token(), "dt01")
	}
	if k.Platform() != "server" {
		t.Errorf("Platform() = %q, want %q", k.Platform(), "server")
	}
	if k.Geo() != "us" {
		t.Errorf("Geo() = %q, want %q", k.Geo(), "us")
	}
	if k.PublicRandom() != "ab3xy9kp2q" {
		t.Errorf("PublicRandom() = %q, want %q", k.PublicRandom(), "ab3xy9kp2q")
	}
	if k.Secret() != "03ee0ef7cd4244bfb8b71fea8f783f53" {
		t.Errorf("Secret() = %q, want %q", k.Secret(), "03ee0ef7cd4244bfb8b71fea8f783f53")
	}
	if k.Hash() != "d4f8a9c12a" {
		t.Errorf("Hash() = %q, want %q", k.Hash(), "d4f8a9c12a")
	}
	if !k.IsFull() {
		t.Error("IsFull() = false for full key")
	}
	if k.String() != testPublicID {
		t.Errorf("String() = %q, want %q (public ID)", k.String(), testPublicID)
	}
}

func TestParse_PublicID(t *testing.T) {
	k, ok := Parse(testPublicID)
	if !ok {
		t.Fatalf("Parse(%q) returned ok=false", testPublicID)
	}
	if k.Raw() != testPublicID {
		t.Errorf("Raw() = %q, want %q", k.Raw(), testPublicID)
	}
	if k.PublicID() != testPublicID {
		t.Errorf("PublicID() = %q, want %q", k.PublicID(), testPublicID)
	}
	if k.Token() != "dt01" {
		t.Errorf("Token() = %q, want %q", k.Token(), "dt01")
	}
	if k.Platform() != "server" {
		t.Errorf("Platform() = %q, want %q", k.Platform(), "server")
	}
	if k.Geo() != "us" {
		t.Errorf("Geo() = %q, want %q", k.Geo(), "us")
	}
	if k.IsFull() {
		t.Error("IsFull() = true for public-only key")
	}
	if k.Secret() != "" || k.Hash() != "" {
		t.Errorf("Secret/Hash should be empty for public-only key, got %q/%q", k.Secret(), k.Hash())
	}
}

func TestParse_Invalid(t *testing.T) {
	for _, s := range []string{"", "bogus", "dt01.server_us_ab3xy9kp2q.short"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) returned ok=true, want false", s)
		}
	}
}

func TestParseFull_RequiresPrivateSegment(t *testing.T) {
	if _, ok := ParseFull(testPublicID); ok {
		t.Errorf("ParseFull(%q) returned ok=true for public-only ID", testPublicID)
	}
	if _, ok := ParseFull(testFullKey); !ok {
		t.Errorf("ParseFull(%q) returned ok=false for full key", testFullKey)
	}
}

func TestKey_StringReturnsSafePublicID(t *testing.T) {
	k, _ := ParseFull(testFullKey)
	formatted := k.String()
	if strings.Contains(formatted, k.Secret()) {
		t.Error("String() leaks private secret")
	}
	if formatted != testPublicID {
		t.Errorf("String() = %q, want public ID %q", formatted, testPublicID)
	}
}

func TestFromBearer(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"Bearer mytoken", "mytoken", true},
		{"Bearer  spaced  ", "spaced", true},
		{"Basic abc123", "", false},
		{"", "", false},
		{"Bearer ", "", false},
		{"bearer mytoken", "", false}, // case-sensitive per spec
	}
	for _, tc := range cases {
		token, ok := FromBearer(tc.header)
		if ok != tc.wantOK || token != tc.wantToken {
			t.Errorf("FromBearer(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.wantToken, tc.wantOK)
		}
	}
}
