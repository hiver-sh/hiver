package remotefs

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}
}

func unauthorizedResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(""))}
}

func TestRetryOn401_NoRetryOnSuccess(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return okResponse(), nil
	})
	rt := &retryOn401RoundTripper{base: base, invalidate: func() {}}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetryOn401_RetriesOnUnauthorized(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return unauthorizedResponse(), nil
		}
		return okResponse(), nil
	})
	var invalidated bool
	rt := &retryOn401RoundTripper{base: base, invalidate: func() { invalidated = true }}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if !invalidated {
		t.Error("invalidate was not called on 401")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after retry", resp.StatusCode)
	}
}

func TestRetryOn401_NoRetryWhenBodyHasNoGetBody(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return unauthorizedResponse(), nil
	})
	rt := &retryOn401RoundTripper{base: base, invalidate: func() {}}
	// Construct request manually so Body is set but GetBody is nil (non-replayable body).
	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	req.Body = io.NopCloser(strings.NewReader("body"))
	// GetBody intentionally left nil.
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-replayable body)", calls)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRetryOn401_RetriesWithBodyWhenGetBodySet(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return unauthorizedResponse(), nil
		}
		return okResponse(), nil
	})
	rt := &retryOn401RoundTripper{base: base, invalidate: func() {}}
	body := "data"
	req, _ := http.NewRequest(http.MethodPut, "http://example.com", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRetryOn401_InvalidatesBeforeRetry(t *testing.T) {
	var order []string
	calls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		order = append(order, "roundtrip")
		if calls == 1 {
			return unauthorizedResponse(), nil
		}
		return okResponse(), nil
	})
	rt := &retryOn401RoundTripper{base: base, invalidate: func() { order = append(order, "invalidate") }}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	resp, _ := rt.RoundTrip(req)
	resp.Body.Close()

	if len(order) != 3 || order[0] != "roundtrip" || order[1] != "invalidate" || order[2] != "roundtrip" {
		t.Errorf("call order = %v, want [roundtrip, invalidate, roundtrip]", order)
	}
}
