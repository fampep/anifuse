// Package protocol defines the resolve JSON the API speaks. It mirrors the
// AniKitty extension protocol so a running AniFuse instance is also a valid
// AniKitty HTTP extension.
package protocol

// ResolveRequest is the POST /v1/resolve body.
type ResolveRequest struct {
	AnilistID int    `json:"anilistId"`
	Episode   int    `json:"episode"`
	Audio     string `json:"audio,omitempty"`
	TitleHint string `json:"titleHint,omitempty"`
	// Provider pins one bundled resolver. Empty asks the registry to try
	// them in order until one has the episode.
	Provider string `json:"provider,omitempty"`
}

// SubtitleTrack is a sidecar caption the player can open.
type SubtitleTrack struct {
	URL      string `json:"url"`
	Label    string `json:"label"`
	Language string `json:"language,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Source is one playable stream: a direct media URL plus the headers its
// CDN asks for.
type Source struct {
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers,omitempty"`
	Label         string            `json:"label,omitempty"`
	Quality       string            `json:"quality,omitempty"`
	Audio         string            `json:"audio,omitempty"`
	QualityRating string            `json:"qualityRating,omitempty"`
	StripBytes    int               `json:"stripBytes,omitempty"`
	Subtitles     []SubtitleTrack   `json:"subtitles,omitempty"`
}

// ResolveResponse is the success body. An empty sources array is a failure.
// Provider names the bundled site that won the race and TookMs the upstream
// resolve time; both are extra fields on top of the AniKitty protocol.
type ResolveResponse struct {
	Sources  []Source `json:"sources"`
	Provider string   `json:"provider,omitempty"`
	TookMs   int64    `json:"tookMs,omitempty"`
	Cached   bool     `json:"cached,omitempty"`
}

// ErrorResponse is the body for every non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ProviderInfo is one entry of GET /v1/providers. The status, successRate,
// attempts and cooldownSeconds fields report live health on top of the
// AniKitty protocol fields.
type ProviderInfo struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	SiteURL         string  `json:"siteUrl,omitempty"`
	Bundled         bool    `json:"bundled"`
	Status          string  `json:"status,omitempty"` // ok | degraded | down
	SuccessRate     float64 `json:"successRate,omitempty"`
	Attempts        int     `json:"attempts,omitempty"`
	CooldownSeconds int     `json:"cooldownSeconds,omitempty"`
}

// ProviderStat is one provider row of GET /metrics.
type ProviderStat struct {
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	Attempts        int     `json:"attempts"`
	Successes       int     `json:"successes"`
	Misses          int     `json:"misses"`
	Errors          int     `json:"errors"`
	SuccessRate     float64 `json:"successRate"`
	CooldownSeconds int     `json:"cooldownSeconds,omitempty"`
}

// RequestMetrics are the counters GET /metrics reports.
type RequestMetrics struct {
	Total           int64 `json:"total"`
	Resolve         int64 `json:"resolve"`
	ResolveCacheHit int64 `json:"resolveCacheHits"`
	Latest          int64 `json:"latest"`
	LatestCacheHit  int64 `json:"latestCacheHits"`
}

// MetricsResponse is the GET /metrics body.
type MetricsResponse struct {
	UptimeSeconds int64          `json:"uptimeSeconds"`
	Requests      RequestMetrics `json:"requests"`
	Providers     []ProviderStat `json:"providers"`
}

// HealthResponse is the GET /healthz body.
type HealthResponse struct {
	Status        string   `json:"status"` // ok | degraded
	Providers     int      `json:"providers"`
	UptimeSeconds int64    `json:"uptimeSeconds"`
	Unhealthy     []string `json:"unhealthy,omitempty"`
	Demo          bool     `json:"demo,omitempty"`
}

// LatestEntry is one row of the aggregated newest-episodes feed. AnilistID
// is set only when the site exposes it on its listing.
type LatestEntry struct {
	Provider  string `json:"provider"`
	Title     string `json:"title"`
	Episode   int    `json:"episode,omitempty"`
	AnilistID int    `json:"anilistId,omitempty"`
	ImageURL  string `json:"imageUrl,omitempty"`
	SiteURL   string `json:"siteUrl,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// LatestResponse is the GET /v1/latest body.
type LatestResponse struct {
	Latest []LatestEntry `json:"latest"`
}
