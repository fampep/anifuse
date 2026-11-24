// Package mkissa resolves mkissa.to episodes through the site's aa-crypto
// lane: a boot token (two HMAC-SHA256 steps over the build mask) opens the
// bootstrap, whose partB makes the AES-256-GCM key. The episode query is
// sealed with it, the reply opened, and a direct MP4/HLS file plays.
// Iframe hosts are skipped.
package mkissa

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fampep/anifuse/internal/httpx"
	"github.com/fampep/anifuse/internal/protocol"
	"github.com/fampep/anifuse/internal/resolve"
)

const (
	// ID is the registry id.
	ID = "mkissa"
	// Name is the display name.
	Name = "mkissa"
	// Origin is the site the resolver scrapes.
	Origin = "https://mkissa.to"
	// UserAgent is the browser the site expects.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"
)

const (
	buildID       = "189"
	lane          = "k7"
	epochMS       = 604_800_000
	queryBucketMS = 300_000
	bootPrefix    = "iGmNgPGdi:"
	host          = "mkissa.to"
	group         = "mkissa"
	apiOrigin     = "https://api.mkissa.net"
	apiPath       = apiOrigin + "/api"
)

const (
	episodeQuery = `query($showId:String!,$translationType:VaildTranslationTypeEnumType!,$episodeString:String!){episode(showId:$showId,translationType:$translationType,episodeString:$episodeString){episodeString sourceUrls show{countryOfOrigin} episodeInfo{vidInforssub vidInforsdub vidInforsraw}}}`
	searchQuery  = `query($search:SearchInput,$limit:Int){shows(search:$search,limit:$limit){edges{_id aniListId availableEpisodesDetail}}}`
)

var masks = []string{"pF98Ab2CCc0=", "gEhpQrtSn+M=", "oPeHj9dBNTU=", "7XcuzKoXhgs="}

// Provider implements providers.Provider for mkissa.to.
type Provider struct {
	client  *httpx.Client
	origin  string
	now     func() int64
	session *session
	mu      sync.Mutex
}

type session struct {
	epoch   int64
	key     []byte
	untilMs int64
}

// New builds the provider with default timeouts.
func New() *Provider {
	return newWithOrigin(httpx.NewClient(UserAgent, 25*time.Second), Origin)
}

func newWithOrigin(client *httpx.Client, origin string) *Provider {
	return &Provider{client: client, origin: origin, now: func() int64 { return time.Now().UnixMilli() }}
}

func (p *Provider) ID() string      { return ID }
func (p *Provider) Name() string    { return Name }
func (p *Provider) SiteURL() string { return Origin }

// Resolve turns an AniList id + episode into a playable file.
func (p *Provider) Resolve(ctx context.Context, req resolve.Request) ([]protocol.Source, error) {
	if req.AnilistID <= 0 || req.Episode <= 0 {
		return nil, resolve.Missing(Name, "anilistId and episode are required")
	}
	mode := "sub"
	if strings.EqualFold(req.Audio, "dub") {
		mode = "dub"
	} else if strings.EqualFold(req.Audio, "raw") {
		mode = "raw"
	}
	found, err := p.findShow(ctx, req.AnilistID, req.Episode, mode, req.TitleHint)
	if err != nil {
		return nil, err
	}
	opened, err := p.openEpisode(ctx, found, mode)
	if err != nil {
		return nil, err
	}
	source := directSource(opened, mode)
	if source == nil {
		return nil, resolve.Missing(Name, "no direct file")
	}
	return []protocol.Source{*source}, nil
}

func (p *Provider) findShow(ctx context.Context, anilistID, episode int, mode, titleHint string) (*showHit, error) {
	title := strings.TrimSpace(titleHint)
	if title == "" {
		return nil, resolve.Missing(Name, "needs a title to find this show")
	}
	variables := map[string]any{
		"search": map[string]any{"query": title, "sortBy": "Name_ASC", "sortDirection": "ASC"},
		"limit":  20,
	}
	page, err := p.post(ctx, apiPath, graphql(searchQuery, variables, nil))
	if err != nil {
		return nil, err
	}
	found := pickShow(page, anilistID, episode, mode)
	if found == nil {
		return nil, resolve.Missing(Name, "title not found")
	}
	return found, nil
}

func (p *Provider) openEpisode(ctx context.Context, found *showHit, mode string) (map[string]any, error) {
	lane, err := p.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	variables := map[string]any{
		"showId":          found.id,
		"translationType": mode,
		"episodeString":   found.episodeString,
	}
	hash := sha256Hex(episodeQuery)
	stamp := p.now() / queryBucketMS * queryBucketMS
	token, err := seal(lane.key, hash, lane.epoch, stamp)
	if err != nil {
		return nil, resolve.Blocked(Name, "seal failed")
	}
	extensions := map[string]any{
		"persistedQuery": map[string]any{"version": 1, "sha256Hash": hash},
		"k":              lane,
		"aaReq":          token,
	}
	page, err := p.post(ctx, apiPath, graphql(episodeQuery, variables, extensions))
	if err != nil {
		return nil, err
	}
	opened, err := unwrapEpisode(page, lane.key)
	if err != nil || opened == nil {
		// The lane rotated under us; retry once with a fresh bootstrap.
		p.mu.Lock()
		p.session = nil
		p.mu.Unlock()
		fresh, sessionErr := p.sessionFor(ctx)
		if sessionErr != nil {
			return nil, resolve.Missing(Name, "episode not open")
		}
		token, _ = seal(fresh.key, hash, fresh.epoch, stamp)
		extensions["aaReq"] = token
		page, err = p.post(ctx, apiPath, graphql(episodeQuery, variables, extensions))
		if err != nil {
			return nil, err
		}
		opened, err = unwrapEpisode(page, fresh.key)
		if err != nil || opened == nil {
			return nil, resolve.Missing(Name, "episode not open")
		}
	}
	return opened, nil
}

func (p *Provider) sessionFor(ctx context.Context) (*session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clock := p.now()
	if p.session != nil && clock < p.session.untilMs-60_000 {
		return p.session, nil
	}
	epoch := clock / epochMS
	opened, err := p.bootstrap(ctx, epoch)
	if err != nil {
		return nil, err
	}
	if opened == nil {
		opened, err = p.bootstrap(ctx, epoch-1)
		if err != nil {
			return nil, err
		}
	}
	if opened == nil {
		return nil, resolve.Blocked(Name, "no bootstrap session")
	}
	p.session = opened
	return opened, nil
}

// bootstrap returns nil (no error) when the epoch token is rejected, so
// the caller can fall back to the previous epoch.
func (p *Provider) bootstrap(ctx context.Context, epoch int64) (*session, error) {
	if epoch < 0 {
		return nil, nil
	}
	token := bootToken(epoch, host, group)
	page, err := p.client.GetWithHeaders(ctx, apiOrigin+"/client-crypto/v1/bootstrap?buildId="+buildID+"&k="+lane, map[string]string{
		"Accept":     "application/json",
		"Origin":     p.origin,
		"Referer":    p.origin + "/",
		"x-build-id": buildID,
		"x-aa-boot":  token,
	})
	if err != nil {
		return nil, resolve.Blocked(Name, err.Error())
	}
	if page.Code == 403 && strings.Contains(page.Body, "invalid_boot_token") {
		return nil, nil
	}
	if page.Code < 200 || page.Code > 299 {
		return nil, resolve.Blocked(Name, "bootstrap status "+strconv.Itoa(page.Code))
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(page.Body), &root); err != nil {
		return nil, resolve.Blocked(Name, "unreadable bootstrap")
	}
	partB := str(root["partB"])
	if partB == "" || str(root["k"]) != lane {
		return nil, resolve.Blocked(Name, "bad bootstrap payload")
	}
	key, err := aesKey(partB)
	if err != nil {
		return nil, resolve.Blocked(Name, "bad bootstrap key")
	}
	until := int64(intNum(root["switchAt"]))
	if until == 0 {
		until = p.now() + epochMS
	}
	return &session{epoch: int64(intNum(root["epoch"])), key: key, untilMs: until}, nil
}

func (p *Provider) post(ctx context.Context, url, body string) (string, error) {
	page, err := p.client.PostJSON(ctx, url, body, map[string]string{
		"Origin":     p.origin,
		"Referer":    p.origin + "/",
		"x-build-id": buildID,
	})
	if err != nil {
		return "", resolve.Blocked(Name, err.Error())
	}
	if page.Code == 404 {
		return "", resolve.Missing(Name, "not found")
	}
	if page.Code < 200 || page.Code > 299 {
		return "", resolve.Blocked(Name, "status "+strconv.Itoa(page.Code))
	}
	return page.Body, nil
}

// ---- crypto ----

func maskKey() []byte {
	id := []byte(buildID)
	mixed := make([]byte, 32)
	for i := range mixed {
		salt := byte((i*175 + 202) & 0xFF)
		mixed[i] = id[i%len(id)] ^ salt
	}
	out := make([]byte, 32)
	for piece, encoded := range masks {
		frag, _ := base64.StdEncoding.DecodeString(encoded)
		for g := range frag {
			i := piece*8 + g
			salt := byte((piece*129 + g*61) & 0xFF)
			out[i] = frag[g] ^ mixed[i] ^ salt
		}
	}
	return out
}

func bootToken(epoch int64, host, group string) string {
	first := hmacSum(maskKey(), []byte(bootPrefix+buildID))
	message := strconv.FormatInt(epoch, 10) + "|" + lane + "|" + group + "|" + host + "|" + buildID
	return hex.EncodeToString(hmacSum(first, []byte(message)))
}

func aesKey(partB string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(partB)
	if err != nil || len(raw) < 32 {
		return nil, resolve.Missing(Name, "could not open this episode")
	}
	mask := maskKey()
	out := make([]byte, 32)
	for i := range out {
		out[i] = raw[i] ^ mask[i%len(mask)]
	}
	return out, nil
}

func seal(key []byte, queryHash string, epoch, timestampMS int64) (string, error) {
	payload := payload(queryHash, epoch, timestampMS)
	full := sha256.Sum256([]byte(strconv.FormatInt(epoch, 10) + ":" + buildID + ":" + queryHash + ":" + strconv.FormatInt(timestampMS, 10) + ":" + lane))
	iv := full[:12]
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	body := gcm.Seal(nil, iv, []byte(payload), nil)
	wire := make([]byte, 1+len(iv)+len(body))
	wire[0] = 1
	copy(wire[1:], iv)
	copy(wire[13:], body)
	return base64.StdEncoding.EncodeToString(wire), nil
}

func open(key []byte, packed string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(packed)
	if err != nil || len(blob) <= 13+16 || blob[0] != 1 {
		return "", resolve.Missing(Name, "could not open this episode")
	}
	iv := blob[1:13]
	body := blob[13:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, body, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func payload(queryHash string, epoch, timestampMS int64) string {
	return `{"v":1,"ts":` + strconv.FormatInt(timestampMS, 10) +
		`,"epoch":` + strconv.FormatInt(epoch, 10) +
		`,"buildId":"` + buildID + `","qh":"` + queryHash + `","k":"` + lane + `"}`
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func hmacSum(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return mac.Sum(nil)
}

// ---- parsers ----

type showHit struct {
	id            string
	episodeString string
}

func pickShow(payload string, anilistID, episode int, mode string) *showHit {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil
	}
	edges := walkItems(root, "data", "shows", "edges")
	want := strconv.Itoa(anilistID)
	var fallback *showHit
	for _, edge := range edges {
		obj, ok := edge.(map[string]any)
		if !ok {
			continue
		}
		if str(obj["aniListId"]) != want {
			continue
		}
		id := str(obj["_id"])
		if id == "" {
			continue
		}
		if listed := episodeStringFor(obj["availableEpisodesDetail"], mode, episode); listed != "" {
			return &showHit{id: id, episodeString: listed}
		}
		if fallback == nil {
			fallback = &showHit{id: id, episodeString: strconv.Itoa(episode)}
		}
	}
	return fallback
}

func episodeStringFor(detail any, mode string, episode int) string {
	obj, ok := detail.(map[string]any)
	if !ok {
		return ""
	}
	rows, ok := obj[mode].([]any)
	if !ok {
		return ""
	}
	want := strconv.Itoa(episode)
	for _, row := range rows {
		if str(row) == want {
			return want
		}
	}
	return ""
}

func unwrapEpisode(payload string, key []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return nil, err
	}
	if _, hasErrors := root["errors"].([]any); hasErrors {
		if data, ok := root["data"].(map[string]any); !ok || !hasKey(data, "tobeparsed") {
			return nil, resolve.Missing(Name, "no episode")
		}
	}
	data, ok := root["data"].(map[string]any)
	if !ok {
		return nil, resolve.Missing(Name, "no episode")
	}
	packed := str(data["tobeparsed"])
	var plain string
	if packed != "" {
		opened, err := open(key, packed)
		if err != nil {
			return nil, err
		}
		plain = opened
	} else {
		encoded, _ := json.Marshal(data)
		plain = string(encoded)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(plain), &parsed); err != nil {
		return nil, err
	}
	episode, _ := parsed["episode"].(map[string]any)
	if episode == nil {
		return nil, resolve.Missing(Name, "no episode")
	}
	return episode, nil
}

func directSource(episode map[string]any, mode string) *protocol.Source {
	rows, ok := episode["sourceUrls"].([]any)
	if !ok {
		return nil
	}
	var best map[string]any
	bestPriority := -1e18
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok || !directFile(obj) {
			continue
		}
		priority := float64(0)
		if v, ok := obj["priority"].(float64); ok {
			priority = v
		}
		if best == nil || priority > bestPriority {
			best = obj
			bestPriority = priority
		}
	}
	if best == nil {
		return nil
	}
	height := vidHeight(episode, mode)
	quality := heightLabel(height)
	label := quality
	if label == "" {
		label = "mkissa"
	}
	modeWord := strings.ToUpper(mode)
	return &protocol.Source{
		URL: str(best["sourceUrl"]),
		Headers: map[string]string{
			"Referer":    Origin + "/",
			"Origin":     Origin,
			"User-Agent": UserAgent,
		},
		Label:   label,
		Quality: quality,
		Audio:   modeWord,
	}
}

func directFile(row map[string]any) bool {
	raw := str(row["sourceUrl"])
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	if strings.EqualFold(str(row["type"]), "iframe") {
		return false
	}
	ext := strings.ToLower(str(row["fileExtenstion"]))
	fallback := strings.ToLower(str(row["fallBack"]))
	named := ext == "mp4" || ext == "m3u8" || ext == "m4v" || fallback == "mp4" || fallback == "m3u8"
	urlFile := strings.Contains(strings.ToLower(raw), ".m3u8") || strings.Contains(strings.ToLower(raw), ".mp4")
	player := strings.EqualFold(str(row["type"]), "player")
	return (player && (named || urlFile)) || (!player && urlFile)
}

func vidHeight(episode map[string]any, mode string) int {
	info, ok := episode["episodeInfo"].(map[string]any)
	if !ok {
		return 0
	}
	key := "vidInforssub"
	if mode == "dub" {
		key = "vidInforsdub"
	} else if mode == "raw" {
		key = "vidInforsraw"
	}
	row, ok := info[key].(map[string]any)
	if !ok {
		return 0
	}
	return intNum(row["vidResolution"])
}

func heightLabel(h int) string {
	switch {
	case h >= 2000:
		return "4K"
	case h >= 360:
		return strconv.Itoa(h) + "p"
	default:
		return ""
	}
}

func graphql(query string, variables, extensions map[string]any) string {
	body := map[string]any{"query": query, "variables": variables}
	if extensions != nil {
		body["extensions"] = extensions
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func walkItems(root map[string]any, path ...string) []any {
	if len(path) == 0 {
		return nil
	}
	value, ok := root[path[0]]
	if !ok {
		return nil
	}
	if len(path) == 1 {
		if list, ok := value.([]any); ok {
			return list
		}
		return nil
	}
	if obj, ok := value.(map[string]any); ok {
		return walkItems(obj, path[1:]...)
	}
	return nil
}

func hasKey(obj map[string]any, key string) bool {
	_, ok := obj[key]
	return ok
}

func str(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func intNum(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}
