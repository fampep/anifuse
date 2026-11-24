// Package httpx is the shared HTTP client every provider uses.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client fetches pages with a browser User-Agent and bounded timeouts.
type Client struct {
	UserAgent string
	http      *http.Client
}

// NewClient builds a client with the given user agent and overall timeout.
func NewClient(userAgent string, timeout time.Duration) *Client {
	return &Client{
		UserAgent: userAgent,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 8 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}
}

// Page is a fetched body with its status code.
type Page struct {
	Code int
	Body string
}

// GetText fetches url with a browser User-Agent and the given Referer.
func (c *Client) GetText(ctx context.Context, url, referer string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Some sites reject the default Go Accept-Encoding: gzip that curl does
	// not send; speak plain identity like a bare browser probe.
	req.Header.Set("Accept-Encoding", "identity")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Page{}, err
	}
	return Page{Code: resp.StatusCode, Body: string(body)}, nil
}

// PostJSON POSTs a JSON body with the browser User-Agent and extra headers.
func (c *Client) PostJSON(ctx context.Context, url, body string, headers map[string]string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Page{}, err
	}
	return Page{Code: resp.StatusCode, Body: string(data)}, nil
}

// GetWithHeaders fetches a page with explicit headers plus the browser UA.
func (c *Client) GetWithHeaders(ctx context.Context, url string, headers map[string]string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Page{}, err
	}
	return Page{Code: resp.StatusCode, Body: string(body)}, nil
}

// Challenge reports whether a page or status looks like a bot wall.
func Challenge(code int, body string) bool {
	if code == 403 || code == 429 || code == 503 {
		return true
	}
	head := strings.ToLower(body)
	if len(head) > 5000 {
		head = head[:5000]
	}
	return strings.Contains(head, "just a moment") ||
		strings.Contains(head, "cf-browser-verification") ||
		strings.Contains(head, "challenge-platform") ||
		strings.Contains(head, "attention required")
}

// Origin returns scheme://host of a URL, or "" when it cannot be parsed.
func Origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Hostname())
}
