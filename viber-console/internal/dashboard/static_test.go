package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticHandlerServesIndex(t *testing.T) {
	h := StaticHandler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 (body %.100s)", rec.Code, rec.Body.String())
	}
	if len(rec.Body.Bytes()) == 0 {
		t.Fatal("empty body")
	}
}

func TestStaticHandlerServesCSS(t *testing.T) {
	h := StaticHandler()

	req := httptest.NewRequest(http.MethodGet, "/static/styles.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET css = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty css body")
	}
}

func TestStaticHandlerServesJS(t *testing.T) {
	h := StaticHandler()
	req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET js = %d, want 200", rec.Code)
	}
}
