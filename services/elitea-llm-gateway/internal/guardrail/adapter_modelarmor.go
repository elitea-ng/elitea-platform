package guardrail

// The Google Cloud Model Armor adapter (ADR-0043 decision 1).
// adapter_cloud.go describes the envelope that all cloud adapters share.
//
// API: https://cloud.google.com/security-command-center/docs/reference/model-armor/rest
//
//	input   POST {endpoint}/v1/{template}:sanitizeUserPrompt
//	        {"userPromptData": {"text": "..."}}
//	output  POST {endpoint}/v1/{template}:sanitizeModelResponse
//	        {"modelResponseData": {"text": "..."}}
//
// The template is projects/P/locations/L/templates/T. The endpoint is the
// regional one, https://modelarmor.L.rep.googleapis.com, unless the detector
// names one.
//
// Auth is an OAuth access token with the cloud-platform scope, from one of
// two sources:
//
//   - auth_mode "key" (default): credentials_secret is a {{secret.NAME}}
//     reference to a Google service-account key document. The token exchange
//     uses the same client as the call. The token_uri host of the document
//     must be on the egress allowlist at the time of EACH exchange. The
//     adapter caches the tokens, not the token sources, in a bounded LRU
//     cache (keyToken).
//   - auth_mode "workload_identity": the gateway's own Google identity (GKE
//     Workload Identity through Application Default Credentials), the
//     keyless mode that Vertex AI platform credentials use
//     (internal/workloadidentity). It works only when
//     GATEWAY_WORKLOAD_IDENTITY_ENABLED is true. A guardrail row is platform
//     configuration (the configuration.governance permission), so it is a
//     platform credential in the sense of that package.
//
// Mapping of the filter results to findings (a result counts when its
// matchState is MATCH_FOUND):
//
//   - rai: one finding per matched type. sexually_explicit is "sexual",
//     hate_speech is "hate", harassment and dangerous keep their name.
//   - pi_and_jailbreak: SlotPromptInjection.
//   - sdp: inspect findings are "pii:<info type>" (for example
//     pii:email_address) with a byte span. The adapter reads byteRange (UTF-8
//     bytes, the encoding of the request) and falls back to codepointRange.
//     A de-identify result gives one Whole finding per info type.
//   - malicious_uris: "malicious_uri". The adapter finds the returned URI in
//     the content to make a span, else the finding is Whole.
//   - csam: "csam".
//
// Score: confidence HIGH 1, MEDIUM_AND_ABOVE 0.66, LOW_AND_ABOVE 0.33; SDP
// likelihood VERY_LIKELY 0.9, LIKELY 0.7, POSSIBLE 0.5, UNLIKELY 0.3,
// VERY_UNLIKELY 0.1; 1 for the other filters. An invocationResult other than
// SUCCESS is an error, so a partial check never passes as clean.

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TypeModelArmor is the detector type of the Google Cloud Model Armor
// adapter.
const TypeModelArmor = "model_armor"

const (
	modelArmorScope           = "https://www.googleapis.com/auth/cloud-platform"
	modelArmorDefaultTokenURI = "https://oauth2.googleapis.com/token"
	modelArmorAuthKey         = "key"
	modelArmorAuthKeyless     = "workload_identity"
	// modelArmorMaxTokens bounds the cached tokens of one detector (LRU).
	modelArmorMaxTokens = 64
	// modelArmorTokenMargin is the validity a cached token must still have.
	modelArmorTokenMargin = time.Minute
)

// modelArmorTemplate is the shape of a template resource name. The location
// goes into the host name, so the shape is a security control.
var modelArmorTemplate = regexp.MustCompile(
	`^projects/[a-z0-9][a-z0-9-]{0,62}/locations/([a-z][a-z0-9-]{0,62})/templates/[A-Za-z0-9_-]{1,63}$`)

// ModelArmorConfig is the authored configuration of a model_armor detector.
type ModelArmorConfig struct {
	Template string
	// Endpoint is the Model Armor origin.
	Endpoint string
	// AuthMode is "key" or "workload_identity".
	AuthMode string
	// CredentialsSecret is a {{secret.NAME}} reference to a service-account
	// key document (key mode only).
	CredentialsSecret string
}

type modelArmorDetector struct {
	cfg ModelArmorConfig

	tokens *tokenCache
}

func newModelArmorDetector(obj map[string]any) (detectorImpl, error) {
	var cfg ModelArmorConfig
	cfg.Template, _ = obj["template"].(string)
	m := modelArmorTemplate.FindStringSubmatch(cfg.Template)
	if m == nil {
		return nil, errors.New("template must be projects/PROJECT/locations/LOCATION/templates/TEMPLATE")
	}
	cfg.Endpoint = "https://modelarmor." + m[1] + ".rep.googleapis.com"
	if raw, ok := obj["endpoint"].(string); ok && raw != "" {
		endpoint, err := cloudEndpoint("endpoint", raw)
		if err != nil {
			return nil, err
		}
		cfg.Endpoint = endpoint
	}
	cfg.AuthMode, _ = obj["auth_mode"].(string)
	if cfg.AuthMode == "" {
		cfg.AuthMode = modelArmorAuthKey
	}
	var err error
	if cfg.CredentialsSecret, err = secretRef(obj, "credentials_secret"); err != nil {
		return nil, err
	}
	switch cfg.AuthMode {
	case modelArmorAuthKey:
		if cfg.CredentialsSecret == "" {
			return nil, errors.New(`credentials_secret is required when auth_mode is "key"`)
		}
	case modelArmorAuthKeyless:
		if cfg.CredentialsSecret != "" {
			return nil, errors.New(`credentials_secret must be empty when auth_mode is "workload_identity"`)
		}
	default:
		return nil, fmt.Errorf("auth_mode must be %q or %q", modelArmorAuthKey, modelArmorAuthKeyless)
	}
	return &modelArmorDetector{cfg: cfg, tokens: newTokenCache(modelArmorMaxTokens)}, nil
}

// ModelArmorDataItem is the DataItem of a sanitize request.
type ModelArmorDataItem struct {
	Text string `json:"text"`
}

// ModelArmorRequest is the body of sanitizeUserPrompt (UserPromptData) and of
// sanitizeModelResponse (ModelResponseData).
type ModelArmorRequest struct {
	UserPromptData    *ModelArmorDataItem `json:"userPromptData,omitempty"`
	ModelResponseData *ModelArmorDataItem `json:"modelResponseData,omitempty"`
}

// modelArmorRange is a RangeInfo. Google encodes int64 as a JSON string.
type modelArmorRange struct {
	Start json.Number `json:"start"`
	End   json.Number `json:"end"`
}

func (r *modelArmorRange) ints() (int64, int64, bool) {
	if r == nil {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(string(r.Start), 10, 64)
	if r.Start == "" {
		s, err1 = 0, nil
	}
	e, err2 := strconv.ParseInt(string(r.End), 10, 64)
	return s, e, err1 == nil && err2 == nil && r.End != ""
}

// modelArmorLocation is the location of an SDP finding.
type modelArmorLocation struct {
	ByteRange      *modelArmorRange `json:"byteRange"`
	CodepointRange *modelArmorRange `json:"codepointRange"`
}

type modelArmorState struct {
	MatchState      string `json:"matchState"`
	ConfidenceLevel string `json:"confidenceLevel"`
}

func (s *modelArmorState) matched() bool { return s != nil && s.MatchState == "MATCH_FOUND" }

// ModelArmorResponse is the part of the sanitize response the adapter reads.
// The URI of a malicious URI match is decoded to locate it, then dropped.
type ModelArmorResponse struct {
	SanitizationResult struct {
		FilterMatchState string `json:"filterMatchState"`
		InvocationResult string `json:"invocationResult"`
		FilterResults    map[string]struct {
			RAIFilterResult *struct {
				modelArmorState
				RAIFilterTypeResults map[string]modelArmorState `json:"raiFilterTypeResults"`
			} `json:"raiFilterResult"`
			PIAndJailbreakFilterResult *modelArmorState `json:"piAndJailbreakFilterResult"`
			SDPFilterResult            *struct {
				InspectResult *struct {
					modelArmorState
					Findings []struct {
						InfoType   string              `json:"infoType"`
						Likelihood string              `json:"likelihood"`
						Location   *modelArmorLocation `json:"location"`
					} `json:"findings"`
				} `json:"inspectResult"`
				DeidentifyResult *struct {
					modelArmorState
					InfoTypes []string `json:"infoTypes"`
				} `json:"deidentifyResult"`
			} `json:"sdpFilterResult"`
			MaliciousURIFilterResult *struct {
				modelArmorState
				MaliciousURIMatchedItems []struct {
					URI string `json:"uri"`
				} `json:"maliciousUriMatchedItems"`
			} `json:"maliciousUriFilterResult"`
			CSAMFilterResult *modelArmorState `json:"csamFilterFilterResult"`
		} `json:"filterResults"`
	} `json:"sanitizationResult"`
}

func modelArmorConfidence(c string) float64 {
	switch c {
	case "LOW_AND_ABOVE":
		return 0.33
	case "MEDIUM_AND_ABOVE":
		return 0.66
	}
	return 1
}

func modelArmorLikelihood(l string) float64 {
	switch l {
	case "VERY_UNLIKELY":
		return 0.1
	case "UNLIKELY":
		return 0.3
	case "LIKELY":
		return 0.7
	case "VERY_LIKELY":
		return 0.9
	}
	// POSSIBLE, and UNSPECIFIED, which Model Armor reads as POSSIBLE.
	return 0.5
}

var modelArmorRAILabels = map[string]string{
	"sexually_explicit": "sexual",
	"hate_speech":       "hate",
	"harassment":        "harassment",
	"dangerous":         "dangerous",
}

// modelArmorMaxChars is the text of one sanitize call. The Model Armor
// filters read at most 65 536 tokens (Sensitive Data Protection: 0.5 MB),
// and skip the filter when the text is longer. 32 000 characters stay below
// that limit also at two tokens per character. The pipeline sends units of
// at most this size (units.go).
const modelArmorMaxChars = 32000

func (d *modelArmorDetector) textLimit() int { return modelArmorMaxChars }

func (d *modelArmorDetector) detect(ctx context.Context, env Env, stage, content string) ([]Finding, error) {
	req := ModelArmorRequest{UserPromptData: &ModelArmorDataItem{Text: content}}
	method := "sanitizeUserPrompt"
	if stage == StageOutput {
		req = ModelArmorRequest{ModelResponseData: &ModelArmorDataItem{Text: content}}
		method = "sanitizeModelResponse"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", errAdapter)
	}
	target := d.cfg.Endpoint + "/v1/" + d.cfg.Template + ":" + method
	// The allowlist is checked before a token is minted, so a refused host
	// costs no token exchange.
	if env.EgressAllows == nil || !env.EgressAllows(target) {
		return nil, fmt.Errorf("%w: the adapter host is not on the egress allowlist", errAdapter)
	}
	token, err := d.token(ctx, env)
	if err != nil {
		return nil, err
	}
	raw, err := egressPost(ctx, env, target, body, func(r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var resp ModelArmorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: response is not the sanitize shape", errAdapter)
	}
	if r := resp.SanitizationResult.InvocationResult; r != "" && r != "SUCCESS" {
		return nil, fmt.Errorf("%w: invocation result %s", errAdapter, vendorLabel("", r, "unknown"))
	}
	out := modelArmorFindings(&resp, content)
	if len(out) > maxAdapterFindings {
		return nil, fmt.Errorf("%w: response has more than %d findings", errAdapter, maxAdapterFindings)
	}
	return out, nil
}

func modelArmorFindings(resp *ModelArmorResponse, content string) []Finding {
	var out []Finding
	// Map order is random. The keys are sorted so the findings, and so the
	// label of a merged mask, do not change between runs.
	keys := make([]string, 0, len(resp.SanitizationResult.FilterResults))
	for k := range resp.SanitizationResult.FilterResults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fr := resp.SanitizationResult.FilterResults[k]
		if r := fr.RAIFilterResult; r != nil {
			matchedType := false
			names := make([]string, 0, len(r.RAIFilterTypeResults))
			for name := range r.RAIFilterTypeResults {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				t := r.RAIFilterTypeResults[name]
				if !t.matched() {
					continue
				}
				matchedType = true
				label, ok := modelArmorRAILabels[name]
				if !ok {
					label = vendorLabel("rai:", name, "rai")
				}
				out = append(out, Finding{Whole: true, Label: label, Score: modelArmorConfidence(t.ConfidenceLevel)})
			}
			if !matchedType && r.matched() {
				out = append(out, Finding{Whole: true, Label: "rai", Score: 1})
			}
		}
		if r := fr.PIAndJailbreakFilterResult; r.matched() {
			out = append(out, Finding{Whole: true, Label: SlotPromptInjection, Score: modelArmorConfidence(r.ConfidenceLevel)})
		}
		if r := fr.SDPFilterResult; r != nil {
			if in := r.InspectResult; in != nil {
				for _, f := range in.Findings {
					label := vendorLabel("pii:", f.InfoType, "pii")
					score := modelArmorLikelihood(f.Likelihood)
					if b, ok := modelArmorSpan(content, f.Location); ok {
						if b.end > b.start {
							out = append(out, Finding{Start: b.start, End: b.end, Label: label, Score: score})
						}
						continue
					}
					out = append(out, Finding{Whole: true, Label: label, Score: score})
				}
				if len(in.Findings) == 0 && in.matched() {
					out = append(out, Finding{Whole: true, Label: "pii", Score: 1})
				}
			}
			if de := r.DeidentifyResult; de != nil && de.matched() {
				for _, t := range de.InfoTypes {
					out = append(out, Finding{Whole: true, Label: vendorLabel("pii:", t, "pii"), Score: 1})
				}
				if len(de.InfoTypes) == 0 {
					out = append(out, Finding{Whole: true, Label: "pii", Score: 1})
				}
			}
		}
		if r := fr.MaliciousURIFilterResult; r != nil && r.matched() {
			n := len(out)
			for _, item := range r.MaliciousURIMatchedItems {
				if spans := locateAll(content, item.URI, false); len(spans) > 0 {
					out = append(out, spanFindings(spans, "malicious_uri", 1)...)
				}
			}
			if len(out) == n {
				out = append(out, Finding{Whole: true, Label: "malicious_uri", Score: 1})
			}
		}
		if fr.CSAMFilterResult.matched() {
			out = append(out, Finding{Whole: true, Label: "csam", Score: 1})
		}
	}
	if len(out) == 0 && resp.SanitizationResult.FilterMatchState == "MATCH_FOUND" {
		out = append(out, Finding{Whole: true, Label: "model_armor_match", Score: 1})
	}
	return out
}

// modelArmorSpan reads the byte span of an SDP finding: byteRange first,
// then codepointRange. ok is false when neither is usable.
func modelArmorSpan(content string, loc *modelArmorLocation) (bound, bool) {
	if loc == nil {
		return bound{}, false
	}
	if s, e, ok := loc.ByteRange.ints(); ok {
		if b, ok := byteSpan(content, s, e); ok {
			return b, true
		}
	}
	if s, e, ok := loc.CodepointRange.ints(); ok {
		return codePointSpan(content, s, e)
	}
	return bound{}, false
}

// serviceAccountDoc is the part of a key document the adapter checks itself.
type serviceAccountDoc struct {
	Type     string `json:"type"`
	TokenURI string `json:"token_uri"`
}

// token returns an access token for the detector's auth mode.
func (d *modelArmorDetector) token(ctx context.Context, env Env) (string, error) {
	if d.cfg.AuthMode == modelArmorAuthKeyless {
		if env.GoogleAccessToken == nil {
			return "", fmt.Errorf("%w: workload identity is not enabled on this gateway", errAdapter)
		}
		tok, err := env.GoogleAccessToken(ctx)
		if err != nil || tok == "" {
			return "", fmt.Errorf("%w: the gateway could not get a token from its workload identity", errAdapter)
		}
		return tok, nil
	}
	doc, err := resolveSecret(ctx, env, "credentials_secret", d.cfg.CredentialsSecret)
	if err != nil {
		return "", err
	}
	return d.keyToken(ctx, env, doc)
}

// keyToken returns an access token for a key document. A cached token that
// is valid for at least modelArmorTokenMargin is used as it is. Otherwise
// the adapter makes a token exchange with THIS request's env: the token_uri
// must be on the egress allowlist at the time of each exchange, and the
// exchange uses env.HTTPClient. The exchanges of one key are serialised, so
// parallel unit calls make one exchange.
func (d *modelArmorDetector) keyToken(ctx context.Context, env Env, doc string) (string, error) {
	sum := sha256.Sum256([]byte(doc))
	entry := d.tokens.entry(hex.EncodeToString(sum[:]))
	select {
	case entry.lock <- struct{}{}:
	case <-ctx.Done():
		return "", fmt.Errorf("%w: timeout", errAdapter)
	}
	defer func() { <-entry.lock }()
	if tok := entry.tok; tok != nil && time.Until(tok.Expiry) > modelArmorTokenMargin {
		return tok.AccessToken, nil
	}
	var meta serviceAccountDoc
	if err := json.Unmarshal([]byte(doc), &meta); err != nil || meta.Type != "service_account" {
		// The parse error is not returned: it can quote the private key.
		return "", fmt.Errorf("%w: credentials_secret is not a Google service-account key document", errAdapter)
	}
	tokenURI := strings.TrimSpace(meta.TokenURI)
	if tokenURI == "" {
		tokenURI = modelArmorDefaultTokenURI
	}
	if u, err := url.Parse(tokenURI); err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("%w: the token_uri of credentials_secret is not an https URL", errAdapter)
	}
	if env.HTTPClient == nil || env.EgressAllows == nil || !env.EgressAllows(tokenURI) {
		return "", fmt.Errorf("%w: the token_uri host is not on the egress allowlist", errAdapter)
	}
	conf, err := google.JWTConfigFromJSON([]byte(doc), modelArmorScope)
	if err != nil {
		return "", fmt.Errorf("%w: credentials_secret could not be loaded", errAdapter)
	}
	conf.TokenURL = tokenURI
	// The source lives for this exchange only. Its context carries only the
	// HTTP client of this request.
	authCtx := context.WithValue(context.Background(), oauth2.HTTPClient, env.HTTPClient)
	tok, err := awaitToken(ctx, conf.TokenSource(authCtx))
	if err != nil {
		return "", err
	}
	entry.tok = tok
	return tok.AccessToken, nil
}

// tokenEntry is the cached token of one key document. lock is a mutex that
// a waiter can leave when its context ends.
type tokenEntry struct {
	lock chan struct{}
	tok  *oauth2.Token
}

// tokenCache is a bounded LRU cache of token entries, keyed by the hash of a
// key document. A rotated key gets a new entry; the least recently used
// entry goes when the cache is full.
type tokenCache struct {
	mu    sync.Mutex
	limit int
	order *list.List // front: most recently used; values are cacheItem
	items map[string]*list.Element
}

type cacheItem struct {
	key   string
	entry *tokenEntry
}

func newTokenCache(limit int) *tokenCache {
	return &tokenCache{limit: limit, order: list.New(), items: map[string]*list.Element{}}
}

// entry returns the entry of key, and creates it when it is not cached.
func (c *tokenCache) entry(key string) *tokenEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(cacheItem).entry
	}
	e := &tokenEntry{lock: make(chan struct{}, 1)}
	c.items[key] = c.order.PushFront(cacheItem{key: key, entry: e})
	for c.order.Len() > c.limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(cacheItem).key)
	}
	return e
}

// awaitToken reads one token and gives up when ctx ends. An oauth2 token
// source takes no context, so the read runs in a goroutine; the client's own
// timeout bounds that goroutine.
func awaitToken(ctx context.Context, src oauth2.TokenSource) (*oauth2.Token, error) {
	type result struct {
		tok *oauth2.Token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := src.Token()
		done <- result{tok, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.tok == nil || r.tok.AccessToken == "" {
			return nil, fmt.Errorf("%w: the service-account token exchange failed", errAdapter)
		}
		return r.tok, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: timeout", errAdapter)
	}
}
