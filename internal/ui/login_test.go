package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	var l loginLimiter
	now := time.Now()
	for i := 0; i < maxLoginAttempts; i++ {
		if !l.allow("192.0.2.1", now) {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	if l.allow("192.0.2.1", now) {
		t.Fatal("attempt over the limit was accepted")
	}
	if !l.allow("192.0.2.2", now) {
		t.Fatal("another IP was blocked")
	}
	if !l.allow("192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("attempt after the window was rejected")
	}
}

func TestLoginConcurrencyLimit(t *testing.T) {
	u := &UI{loginSem: make(chan struct{}, 1)}
	u.loginSem <- struct{}{}
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	w := httptest.NewRecorder()
	u.login(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}
