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
type ResolveResponse struct {
	Sources []Source `json:"sources"`
}

// ErrorResponse is the body for every non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ProviderInfo is one entry of GET /v1/providers.
type ProviderInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SiteURL  string `json:"siteUrl,omitempty"`
	Bundled  bool   `json:"bundled"`
}
