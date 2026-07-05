package cmd

import (
	"strings"
	"testing"
)

func TestParseGoogleCookieInputJSONFiltersAllowedCookies(t *testing.T) {
	cookies, err := parseGoogleCookieInput(`{
		"SID":"sid",
		"HSID":"hsid",
		"OSID":"osid",
		"SSID":"ssid",
		"APISID":"apisid",
		"SAPISID":"sapisid",
		"__Secure-1PSIDTS":"token",
		"UNRELATED":"drop"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 7 {
		t.Fatalf("expected 7 allowed cookies, got %#v", cookies)
	}
	if cookies["UNRELATED"] != "" {
		t.Fatalf("unexpected unrelated cookie retained: %#v", cookies)
	}
	if cookies["SAPISID"] != "sapisid" {
		t.Fatalf("SAPISID = %q", cookies["SAPISID"])
	}
}

func TestParseGoogleCookieInputCurlHeader(t *testing.T) {
	input := `curl 'https://messages.google.com/web/config' \
  -H 'Cookie: SID=sid; HSID=hsid; OSID=osid; SSID=ssid; APISID=apisid; SAPISID=sapisid; EXTRA=drop'`
	cookies, err := parseGoogleCookieInput(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredGoogleCookies {
		if cookies[name] == "" {
			t.Fatalf("%s missing from %#v", name, cookies)
		}
	}
	if _, ok := cookies["EXTRA"]; ok {
		t.Fatalf("unexpected extra cookie retained: %#v", cookies)
	}
}

func TestParseGoogleCookieInputFetchHeader(t *testing.T) {
	input := `fetch("https://messages.google.com/web/config", {
  "headers": {
    "accept": "*/*",
    "cookie": "SID=sid; HSID=hsid; OSID=osid; SSID=ssid; APISID=apisid; SAPISID=sapisid; EXTRA=drop"
  }
});`
	cookies, err := parseGoogleCookieInput(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredGoogleCookies {
		if cookies[name] == "" {
			t.Fatalf("%s missing from %#v", name, cookies)
		}
	}
	if _, ok := cookies["EXTRA"]; ok {
		t.Fatalf("unexpected extra cookie retained: %#v", cookies)
	}
}

func TestParseGoogleCookieInputMissingRequired(t *testing.T) {
	_, err := parseGoogleCookieInput(`{"SID":"sid"}`)
	if err == nil {
		t.Fatal("expected missing required cookie error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HSID") || !strings.Contains(msg, "SAPISID") {
		t.Fatalf("expected missing names in error, got %q", msg)
	}
	if strings.Contains(msg, "sid") {
		t.Fatalf("error leaked cookie value: %q", msg)
	}
}

func TestParseGoogleCookieInputMissingAllGivesCopyHint(t *testing.T) {
	_, err := parseGoogleCookieInput(`{"foo":"bar"}`)
	if err == nil {
		t.Fatal("expected missing required cookie error")
	}
	if !strings.Contains(err.Error(), "/web/config request") {
		t.Fatalf("expected copy hint, got %q", err.Error())
	}
}
