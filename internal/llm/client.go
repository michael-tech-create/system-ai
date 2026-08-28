package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// defaultModel is a generateContent model known to respond for current API keys.
	// Override with GEMINI_MODEL. Note: some advertised models (e.g. gemini-3.6-flash)
	// may accept the connection and hang — we keep per-request timeouts tight.
	defaultModel = "gemini-3.5-flash"
	defaultAPIURLTemplate = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

	defaultHTTPTimeout = 45 * time.Second
	maxCooldownWaits   = 3 // stop spinning in getNextKey during one Review
)

// Review is the structured shape we ask the model to return.
type Review struct {
	Summary     string       `json:"summary"`
	Issues      []Issue      `json:"issues"`
	Suggestions []Suggestion `json:"suggestions"`
}

type Issue struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

type Suggestion struct {
	Action string `json:"action"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Gemini API request/response structures
type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type generationConfig struct {
	ResponseMimeType string  `json:"responseMimeType"`
	Temperature      float32 `json:"temperature"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

const systemPrompt = `You are a senior software architect reviewing a Go project's structure.
You are given a JSON summary of the project: file paths, package names, imports, and top-level function/type names. You do NOT have the actual source code.

Identify:
- Misplaced files (code that doesn't belong in its current package)
- Circular or suspicious import relationships
- Inconsistent package boundaries
- Naming or structural inconsistencies

Respond with ONLY valid JSON matching this exact shape, no markdown code fences, no extra text:
{
  "summary": "one paragraph overall assessment",
  "issues": [{"type": "...", "description": "...", "files": ["..."]}],
  "suggestions": [{"action": "move|merge|rename", "from": "...", "to": "...", "reason": "..."}]
}`

type keyStatus struct {
	lastFail   time.Time
	cooldown   time.Duration
	permanent  bool // auth failures — do not retry this key
	failStreak int
}

type keyManager struct {
	mu           sync.Mutex
	keys         []string
	status       map[string]*keyStatus
	currentIndex int
}

func newKeyManager(keys []string) *keyManager {
	status := make(map[string]*keyStatus)
	for _, k := range keys {
		status[k] = &keyStatus{cooldown: 15 * time.Second}
	}
	return &keyManager{
		keys:   keys,
		status: status,
	}
}

func (km *keyManager) len() int {
	return len(km.keys)
}

// getNextKey returns the next usable API key, or an error if the context
// is done, every key is permanently failed, or we exceed maxCooldownWaits.
func (km *keyManager) getNextKey(ctx context.Context) (string, int, error) {
	waits := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", -1, err
		}

		km.mu.Lock()
		now := time.Now()
		available := 0
		permanent := 0
		var nextReadyIn time.Duration

		for i := 0; i < len(km.keys); i++ {
			idx := km.currentIndex
			key := km.keys[idx]
			st := km.status[key]
			km.currentIndex = (km.currentIndex + 1) % len(km.keys)

			if st.permanent {
				permanent++
				continue
			}
			available++
			readyIn := st.cooldown - now.Sub(st.lastFail)
			if readyIn <= 0 || st.lastFail.IsZero() {
				km.mu.Unlock()
				return key, idx, nil
			}
			if nextReadyIn == 0 || readyIn < nextReadyIn {
				nextReadyIn = readyIn
			}
		}

		allPermanent := permanent == len(km.keys)
		km.mu.Unlock()

		if allPermanent {
			return "", -1, fmt.Errorf("all API keys are permanently failed (auth/permission)")
		}
		if available == 0 {
			return "", -1, fmt.Errorf("no API keys configured")
		}

		waits++
		if waits > maxCooldownWaits {
			return "", -1, fmt.Errorf("all API keys are cooling down after repeated failures; try again later")
		}

		// Wait only until the soonest key is ready (capped), not a blind 10s loop for ages.
		wait := nextReadyIn
		if wait < time.Second {
			wait = time.Second
		}
		if wait > 15*time.Second {
			wait = 15 * time.Second
		}
		fmt.Fprintf(os.Stderr, "All keys are cooling down. Waiting %s (attempt %d/%d)...\n",
			wait.Round(time.Second), waits, maxCooldownWaits)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", -1, ctx.Err()
		case <-timer.C:
		}
	}
}

func (km *keyManager) markRateLimited(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	st, ok := km.status[key]
	if !ok {
		return
	}
	st.failStreak++
	st.lastFail = time.Now()
	// Short exponential cooldown: 15s, 30s, 60s, 120s capped.
	cooldown := 15 * time.Second * time.Duration(1<<min(st.failStreak-1, 3))
	if cooldown > 2*time.Minute {
		cooldown = 2 * time.Minute
	}
	st.cooldown = cooldown
}

func (km *keyManager) markPermanentFailure(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if st, ok := km.status[key]; ok {
		st.permanent = true
		st.lastFail = time.Now()
	}
}

func (km *keyManager) markSuccess(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if st, ok := km.status[key]; ok {
		st.failStreak = 0
		st.cooldown = 15 * time.Second
		st.lastFail = time.Time{}
	}
}

// Client talks to the Gemini API with multi-key rotation.
type Client struct {
	keys       *keyManager
	httpClient *http.Client
	apiURL     string
	model      string
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithAPIURL overrides the full generateContent URL (mainly for tests).
func WithAPIURL(url string) ClientOption {
	return func(c *Client) {
		c.apiURL = url
	}
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = h
	}
}

// WithModel sets the Gemini model id (e.g. gemini-3.5-flash).
func WithModel(model string) ClientOption {
	return func(c *Client) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		c.model = model
		c.apiURL = fmt.Sprintf(defaultAPIURLTemplate, model)
	}
}

// NewClient builds a Gemini client. Model can be overridden with GEMINI_MODEL.
func NewClient(apiKeys []string, opts ...ClientOption) *Client {
	model := strings.TrimSpace(os.Getenv("GEMINI_MODEL"))
	if model == "" {
		model = defaultModel
	}
	apiURL := strings.TrimSpace(os.Getenv("GEMINI_API_URL"))
	if apiURL == "" {
		apiURL = fmt.Sprintf(defaultAPIURLTemplate, model)
	}

	c := &Client{
		keys:  newKeyManager(apiKeys),
		model: model,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
		apiURL: apiURL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model returns the configured model id (empty if URL was fully overridden).
func (c *Client) Model() string { return c.model }

// APIURL returns the generateContent endpoint in use.
func (c *Client) APIURL() string { return c.apiURL }

func (c *Client) Review(ctx context.Context, projectSummaryJSON string) (*Review, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	reqBody := geminiRequest{
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}},
		Contents:          []geminiContent{{Parts: []geminiPart{{Text: projectSummaryJSON}}}},
		GenerationConfig: &generationConfig{
			ResponseMimeType: "application/json",
			Temperature:      0.1,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	maxRetries := c.keys.len() * 2
	if maxRetries < 3 {
		maxRetries = 3
	}
	if maxRetries > 8 {
		maxRetries = 8 // hard cap so a bad model/network cannot run for an hour
	}

	fmt.Fprintf(os.Stderr, "Calling Gemini model %q ...\n", c.displayModel())

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		key, keyIdx, err := c.keys.getNextKey(ctx)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%v (last error: %w)", err, lastErr)
			}
			return nil, err
		}

		fmt.Fprintf(os.Stderr, "  attempt %d/%d (key #%d)...\n", attempt+1, maxRetries, keyIdx+1)

		rawJSON, err := c.doRequest(ctx, key, keyIdx, bodyBytes)
		if err != nil {
			lastErr = err
			if isFatalKeyError(err) {
				fmt.Fprintf(os.Stderr, "  key #%d permanently failed: %v\n", keyIdx+1, err)
				c.keys.markPermanentFailure(key)
				continue
			}
			if isRetryableError(err) {
				fmt.Fprintf(os.Stderr, "  key #%d retryable failure: %v\n", keyIdx+1, err)
				c.keys.markRateLimited(key)
				continue
			}
			return nil, err
		}

		c.keys.markSuccess(key)

		var review Review
		if err := json.Unmarshal([]byte(rawJSON), &review); err != nil {
			return nil, fmt.Errorf("model did not return valid JSON: %w (raw: %s)", err, trimForErr(rawJSON))
		}
		return &review, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all keys failed or max retries exceeded: %w", lastErr)
	}
	return nil, fmt.Errorf("all keys failed or max retries exceeded")
}

func (c *Client) displayModel() string {
	if c.model != "" {
		return c.model
	}
	return c.apiURL
}

type classifiedError struct {
	msg       string
	retryable bool
	fatalKey  bool
}

func (e *classifiedError) Error() string { return e.msg }

func isRetryableError(err error) bool {
	if ce, ok := err.(*classifiedError); ok {
		return ce.retryable
	}
	return false
}

func isFatalKeyError(err error) bool {
	if ce, ok := err.(*classifiedError); ok {
		return ce.fatalKey
	}
	return false
}

func (c *Client) doRequest(ctx context.Context, key string, keyIdx int, bodyBytes []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Timeouts and transport errors are retryable.
		return "", &classifiedError{
			msg:       fmt.Sprintf("request failed after %s: %v", time.Since(start).Round(time.Millisecond), err),
			retryable: true,
		}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return "", &classifiedError{
			msg:       fmt.Sprintf("read response: %v", err),
			retryable: true,
		}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &classifiedError{
			msg:       fmt.Sprintf("HTTP 429 rate limited (key #%d)", keyIdx+1),
			retryable: true,
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", &classifiedError{
			msg:      fmt.Sprintf("HTTP %d auth/permission denied (key #%d)", resp.StatusCode, keyIdx+1),
			fatalKey: true,
		}
	case resp.StatusCode == http.StatusNotFound:
		// Wrong model id — not worth rotating keys forever.
		var apiResp geminiResponse
		msg := string(respBytes)
		if err := json.Unmarshal(respBytes, &apiResp); err == nil && apiResp.Error != nil {
			msg = apiResp.Error.Message
		}
		return "", fmt.Errorf("model not found (HTTP 404) at %s: %s — set GEMINI_MODEL to a working model (e.g. gemini-3.5-flash)", c.apiURL, trimForErr(msg))
	case resp.StatusCode >= 500:
		return "", &classifiedError{
			msg:       fmt.Sprintf("HTTP %d server error (key #%d): %s", resp.StatusCode, keyIdx+1, trimForErr(string(respBytes))),
			retryable: true,
		}
	case resp.StatusCode != http.StatusOK:
		var apiResp geminiResponse
		if err := json.Unmarshal(respBytes, &apiResp); err == nil && apiResp.Error != nil {
			return "", classifyAPIError(apiResp.Error.Message, apiResp.Error.Status, keyIdx)
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, trimForErr(string(respBytes)))
	}

	var apiResp geminiResponse
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w (raw: %s)", err, trimForErr(string(respBytes)))
	}

	if apiResp.Error != nil {
		return "", classifyAPIError(apiResp.Error.Message, apiResp.Error.Status, keyIdx)
	}

	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return "", &classifiedError{
			msg:       "empty response from api",
			retryable: true,
		}
	}

	return apiResp.Candidates[0].Content.Parts[0].Text, nil
}

func classifyAPIError(message, status string, keyIdx int) error {
	errMsg := strings.ToLower(message + " " + status)
	switch {
	case strings.Contains(errMsg, "quota") ||
		strings.Contains(errMsg, "rate") ||
		strings.Contains(errMsg, "resource_exhausted") ||
		strings.Contains(status, "RESOURCE_EXHAUSTED"):
		return &classifiedError{
			msg:       fmt.Sprintf("quota/rate limit (key #%d): %s", keyIdx+1, message),
			retryable: true,
		}
	case strings.Contains(errMsg, "api key") ||
		strings.Contains(errMsg, "permission") ||
		strings.Contains(errMsg, "unauthenticated") ||
		strings.Contains(errMsg, "unauthorized") ||
		status == "UNAUTHENTICATED" ||
		status == "PERMISSION_DENIED":
		return &classifiedError{
			msg:      fmt.Sprintf("auth error (key #%d): %s", keyIdx+1, message),
			fatalKey: true,
		}
	case strings.Contains(errMsg, "unavailable") ||
		strings.Contains(errMsg, "high demand") ||
		strings.Contains(errMsg, "internal") ||
		status == "UNAVAILABLE" ||
		status == "INTERNAL":
		return &classifiedError{
			msg:       fmt.Sprintf("transient api error (key #%d): %s", keyIdx+1, message),
			retryable: true,
		}
	default:
		return fmt.Errorf("api error (key #%d): %s", keyIdx+1, message)
	}
}

func trimForErr(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
