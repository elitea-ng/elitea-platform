package browserauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCookiePolicyRoundTripAndExactDeletion(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	policy, err := newCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Domain:   "elitea.example.test",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sessionID := canonicalSessionID(7)

	setRecorder := httptest.NewRecorder()
	if err := policy.Set(setRecorder, sessionID); err != nil {
		t.Fatal(err)
	}
	setCookies := setRecorder.Result().Cookies()
	if len(setCookies) != 1 {
		t.Fatalf("set cookies = %d, want 1", len(setCookies))
	}
	set := setCookies[0]
	if set.Name != "centry_auth_session" || set.Value != CookieValuePrefix+sessionID ||
		set.Path != "/" || set.Domain != "elitea.example.test" || !set.HttpOnly ||
		!set.Secure || set.SameSite != http.SameSiteLaxMode || set.MaxAge != 604800 ||
		!set.Expires.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("set cookie = %+v", set)
	}

	request := httptest.NewRequest(http.MethodGet, "/auth/check", nil)
	request.AddCookie(set)
	got, err := policy.Read(request)
	if err != nil || got != sessionID {
		t.Fatalf("Read() = %q, %v; want %q", got, err, sessionID)
	}

	clearRecorder := httptest.NewRecorder()
	if err := policy.Clear(clearRecorder); err != nil {
		t.Fatal(err)
	}
	clearCookies := clearRecorder.Result().Cookies()
	if len(clearCookies) != 1 {
		t.Fatalf("clear cookies = %d, want 1", len(clearCookies))
	}
	clear := clearCookies[0]
	if clear.Name != set.Name || clear.Value != "" || clear.Path != set.Path ||
		clear.Domain != set.Domain || clear.HttpOnly != set.HttpOnly ||
		clear.Secure != set.Secure || clear.SameSite != set.SameSite || clear.MaxAge != -1 ||
		!clear.Expires.Equal(time.Unix(1, 0).UTC()) {
		t.Fatalf("clear cookie = %+v; set cookie = %+v", clear, set)
	}
}

func TestCookiePolicyRequiresExplicitSecureProductionContract(t *testing.T) {
	base := CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	}
	tests := []struct {
		name   string
		mutate func(*CookieConfig)
	}{
		{name: "missing name", mutate: func(config *CookieConfig) { config.Name = "" }},
		{name: "invalid name", mutate: func(config *CookieConfig) { config.Name = "auth session" }},
		{name: "invalid domain", mutate: func(config *CookieConfig) { config.Domain = "host:443" }},
		{name: "insecure production", mutate: func(config *CookieConfig) { config.Secure = false }},
		{name: "implicit same site", mutate: func(config *CookieConfig) { config.SameSite = http.SameSiteDefaultMode }},
		{name: "none without secure", mutate: func(config *CookieConfig) {
			config.SameSite = http.SameSiteNoneMode
			config.Secure = false
			config.Development = true
		}},
		{name: "subsecond lifetime", mutate: func(config *CookieConfig) { config.Lifetime += time.Millisecond }},
		{name: "short lifetime", mutate: func(config *CookieConfig) { config.Lifetime = time.Second }},
		{name: "long lifetime", mutate: func(config *CookieConfig) { config.Lifetime = 31 * 24 * time.Hour }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			if _, err := NewCookiePolicy(config); !errors.Is(err, ErrInvalidCookiePolicy) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidCookiePolicy)
			}
		})
	}

	development := base
	development.Secure = false
	development.Development = true
	if _, err := NewCookiePolicy(development); err != nil {
		t.Fatalf("explicit development policy rejected: %v", err)
	}
}

func TestCookiePolicyRejectsMissingDuplicateLegacyAndMalformedValues(t *testing.T) {
	policy, err := NewCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := CookieValuePrefix + canonicalSessionID(1)
	tests := []struct {
		name    string
		cookies []string
		want    error
	}{
		{name: "missing", want: ErrSessionCookieMissing},
		{name: "current Flask value", cookies: []string{canonicalSessionID(1)}, want: ErrSessionCookieInvalid},
		{name: "future version", cookies: []string{"v2." + canonicalSessionID(1)}, want: ErrSessionCookieInvalid},
		{name: "non canonical ID", cookies: []string{CookieValuePrefix + strings.Repeat("a", 42)}, want: ErrSessionCookieInvalid},
		{name: "duplicate", cookies: []string{valid, valid}, want: ErrSessionCookieInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, value := range test.cookies {
				request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: value})
			}
			if _, err := policy.Read(request); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCookiePolicyRefusesToEmitInvalidSessionID(t *testing.T) {
	policy, err := NewCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := policy.Set(recorder, "attacker-selected"); !errors.Is(err, ErrSessionCookieInvalid) {
		t.Fatalf("error = %v, want %v", err, ErrSessionCookieInvalid)
	}
	if got := recorder.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("Set-Cookie = %q, want none", got)
	}
}

func FuzzCookiePolicyAcceptsOnlyCanonical256BitIDs(f *testing.F) {
	f.Add([]byte{})
	f.Add(bytesOf(1, SessionIDRandomBytes-1))
	f.Add(bytesOf(2, SessionIDRandomBytes))
	f.Add(bytesOf(3, SessionIDRandomBytes+1))

	policy, err := NewCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: time.Hour,
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1024 {
			t.Skip()
		}
		encoded := base64.RawURLEncoding.EncodeToString(raw)
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(&http.Cookie{
			Name:  "centry_auth_session",
			Value: CookieValuePrefix + encoded,
		})
		got, readErr := policy.Read(request)
		if len(raw) == SessionIDRandomBytes {
			if readErr != nil || got != encoded {
				t.Fatalf("Read() = %q, %v; want %q", got, readErr, encoded)
			}
			return
		}
		if !errors.Is(readErr, ErrSessionCookieInvalid) {
			t.Fatalf("Read() error = %v, want %v", readErr, ErrSessionCookieInvalid)
		}
	})
}

func canonicalSessionID(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytesOf(fill, SessionIDRandomBytes))
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}
