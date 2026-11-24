package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyEnvRoutesThroughTheProxy(t *testing.T) {
	var proxied string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = r.URL.String()
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer proxy.Close()

	client := newClient("test-ua", 10*time.Second, proxy.URL)
	page, err := client.GetText(context.Background(), "http://example.test/some/path", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if page.Code != 200 || page.Body != "via proxy" {
		t.Fatalf("page = %+v", page)
	}
	if proxied != "http://example.test/some/path" {
		t.Fatalf("proxied = %q", proxied)
	}
}

func TestProxyEnvRejectsBadValues(t *testing.T) {
	client := newClient("test-ua", 10*time.Second, "ftp://nope")
	_, err := client.GetText(context.Background(), "http://example.test/", "")
	if err == nil {
		t.Fatal("bad proxy scheme accepted")
	}
}
