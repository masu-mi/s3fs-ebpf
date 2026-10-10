package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMakeAuthWrap_EmptyTokenPassesThrough pins Q-07: when no token is
// configured every request must reach the handler unchanged.
func TestMakeAuthWrap_EmptyTokenPassesThrough(t *testing.T) {
	wrap := makeAuthWrap("")
	called := false
	h := wrap(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/policy/reload", nil))

	if !called {
		t.Error("handler not called when token is empty")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestMakeAuthWrap_MissingHeaderReturns401 verifies that a request with
// no Authorization header is rejected when a token is configured.
func TestMakeAuthWrap_MissingHeaderReturns401(t *testing.T) {
	wrap := makeAuthWrap("mysecret")
	h := wrap(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/policy/reload", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestMakeAuthWrap_WrongTokenReturns401 verifies that a wrong Bearer
// value is also rejected.
func TestMakeAuthWrap_WrongTokenReturns401(t *testing.T) {
	wrap := makeAuthWrap("mysecret")
	h := wrap(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/policy/reload", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestMakeAuthWrap_CorrectTokenPasses verifies that the exact
// "Bearer <token>" value grants access to the wrapped handler.
func TestMakeAuthWrap_CorrectTokenPasses(t *testing.T) {
	wrap := makeAuthWrap("mysecret")
	called := false
	h := wrap(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/policy/reload", nil)
	req.Header.Set("Authorization", "Bearer mysecret")
	h(rr, req)

	if !called {
		t.Error("handler not called with correct token")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}
