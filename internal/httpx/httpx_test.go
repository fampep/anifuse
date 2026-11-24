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

func TestOpenStreamsWithoutBodyCap(t *testing.T) {
	// A 5MB body exceeds the Page helpers' 4MB cap; Open must serve it whole.
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = byte(i)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "test-ua" {
			t.Errorf("ua = %q", ua)
		}
		if ref := r.Header.Get("Referer"); ref != "https://example.test/" {
			t.Errorf("referer = %q", ref)
		}
		_, _ = w.Write(big)
	}))
	defer upstream.Close()

	client := newClient("test-ua", 10*time.Second, "")
	resp, err := client.Open(context.Background(), http.MethodGet, upstream.URL, map[string]string{
		"Referer": "https://example.test/",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != len(big) {
		t.Fatalf("body = %d bytes, want %d", len(body), len(big))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
