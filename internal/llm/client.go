package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const apiURL = "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.6-flash:generateContent"

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
		Message string `json:"message"`
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
	lastFail time.Time
	cooldown time.Duration
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
		status[k] = &keyStatus{cooldown: 60 * time.Second}
	}
	return &keyManager{
		keys:   keys,
		status: status,
	}
}

func (km *keyManager) getNextKey() string {
	km.mu.Lock()
	defer km.mu.Unlock()

	for {
		now := time.Now()
		for i := 0; i < len(km.keys); i++ {
			key := km.keys[km.currentIndex]
			status := km.status[key]

			if now.Sub(status.lastFail) >= status.cooldown {
				km.currentIndex = (km.currentIndex + 1) % len(km.keys)
				return key
			}
			km.currentIndex = (km.currentIndex + 1) % len(km.keys)
		}

		// All keys on cooldown, wait briefly and check again
		km.mu.Unlock()
		fmt.Println("All keys are rate-limited. Waiting 10 seconds...")
		time.Sleep(10 * time.Second)
		km.mu.Lock()
	}
}

func (km *keyManager) markFailed(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if status, exists := km.status[key]; exists {
		status.lastFail = time.Now()
	}
}

type Client struct {
	keys       *keyManager
	httpClient *http.Client
}

func NewClient(apiKeys []string) *Client {
	return &Client{
		keys:       newKeyManager(apiKeys),
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) Review(projectSummaryJSON string) (*Review, error) {
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

	var rawJSON string
	maxRetries := len(c.keys.keys) * 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		key := c.keys.getNextKey()
		url := fmt.Sprintf("%s?key=%s", apiURL, key)
		req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}

		respBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		// Catch HTTP-level rate limits (429)
		if resp.StatusCode == http.StatusTooManyRequests {
			fmt.Printf("Key ending in ...%s is rate-limited. Switching...\n", key[max(0, len(key)-6):])
			c.keys.markFailed(key)
			continue
		}

		var apiResp geminiResponse
		if err := json.Unmarshal(respBytes, &apiResp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w (raw: %s)", err, string(respBytes))
		}

		// Catch API-level JSON errors (quotas/rate-limits)
		if apiResp.Error != nil {
			errMsg := strings.ToLower(apiResp.Error.Message)
			if strings.Contains(errMsg, "quota") || strings.Contains(errMsg, "rate") {
				fmt.Printf("Key ending in ...%s exhausted quota. Switching...\n", key[max(0, len(key)-6):])
				c.keys.markFailed(key)
				continue
			}
			return nil, fmt.Errorf("api error: %s", apiResp.Error.Message)
		}

		if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
			return nil, fmt.Errorf("empty response from api")
		}

		rawJSON = apiResp.Candidates[0].Content.Parts[0].Text
		break
	}

	if rawJSON == "" {
		return nil, fmt.Errorf("all keys failed or max retries exceeded")
	}

	var review Review
	if err := json.Unmarshal([]byte(rawJSON), &review); err != nil {
		return nil, fmt.Errorf("model did not return valid JSON: %w (raw: %s)", err, rawJSON)
	}

	return &review, nil
}

// func max(a, b int) int {
// 	if a > b {
// 		return a
// 	}
// 	return b
// }
