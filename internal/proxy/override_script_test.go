package proxy

import (
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func runScript(t *testing.T, body, script string, hdr map[string]string) (string, map[string]string, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://accounts.google.com/v3/signin/challenge/pwd?a=1", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	err := runOverrideScript(r, script)
	out, _ := io.ReadAll(r.Body)
	got := map[string]string{}
	for k := range r.Header {
		got[k] = r.Header.Get(k)
	}
	// Content-Length header should track the rewritten body on success.
	if err == nil {
		if want := strconv.Itoa(len(out)); r.Header.Get("Content-Length") != want {
			t.Fatalf("Content-Length = %q, want %q", r.Header.Get("Content-Length"), want)
		}
		if r.ContentLength != int64(len(out)) {
			t.Fatalf("r.ContentLength = %d, want %d", r.ContentLength, len(out))
		}
	}
	return string(out), got, err
}

func TestOverrideScriptBodySubstitute(t *testing.T) {
	body, _, err := runScript(t, `f.req=%5B%22====%22%5D`, `body = body:gsub("====", "s3cret")`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `f.req=%5B%22s3cret%22%5D`
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestOverrideScriptURLHelpers(t *testing.T) {
	// urldecode("a%20b")="a b" -> urlencode -> "a+b"; urldecode("x%3Dy")="x=y".
	body, _, err := runScript(t, "", `body = urlencode(urldecode("a%20b")) .. "|" .. urldecode("x%3Dy")`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != "a+b|x=y" {
		t.Fatalf("body = %q, want a+b|x=y", body)
	}
}

func TestOverrideScriptHeadersSetAndDelete(t *testing.T) {
	_, hdr, err := runScript(t, "x", `
		headers["Authorization"] = "Bearer abc"
		headers["X-Remove-Me"] = nil
	`, map[string]string{"X-Remove-Me": "gone", "Keep": "yes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hdr["Authorization"] != "Bearer abc" {
		t.Fatalf("Authorization = %q, want Bearer abc", hdr["Authorization"])
	}
	if _, ok := hdr["X-Remove-Me"]; ok {
		t.Fatalf("X-Remove-Me should have been deleted, got %q", hdr["X-Remove-Me"])
	}
	if hdr["Keep"] != "yes" {
		t.Fatalf("Keep = %q, want yes", hdr["Keep"])
	}
}

func TestOverrideScriptReadOnlyGlobals(t *testing.T) {
	body, _, err := runScript(t, "orig", `
		body = method .. " " .. host .. " " .. path .. " " .. query
	`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "POST accounts.google.com /v3/signin/challenge/pwd a=1"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestOverrideScriptErrorLeavesRequestIntact(t *testing.T) {
	body, hdr, err := runScript(t, "keepme", `error("boom")`, map[string]string{"Keep": "yes"})
	if err == nil {
		t.Fatal("expected an error from the script")
	}
	if body != "keepme" {
		t.Fatalf("body = %q, want unchanged keepme", body)
	}
	if hdr["Keep"] != "yes" {
		t.Fatalf("Keep header should be untouched, got %q", hdr["Keep"])
	}
}

func TestOverrideScriptSandboxNoOSLib(t *testing.T) {
	_, _, err := runScript(t, "x", `local t = os.time()`, nil)
	if err == nil {
		t.Fatal("expected error: os library must not be available")
	}
}

func TestOverrideScriptTimeout(t *testing.T) {
	body, _, err := runScript(t, "x", `while true do end`, nil)
	if err == nil {
		t.Fatal("expected timeout error from infinite loop")
	}
	if body != "x" {
		t.Fatalf("body = %q, want unchanged x", body)
	}
}

func TestOverrideScriptB64(t *testing.T) {
	body, _, err := runScript(t, "", `body = b64encode("hi") .. ":" .. b64decode("aGk=")`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != "aGk=:hi" {
		t.Fatalf("body = %q, want aGk=:hi", body)
	}
}
