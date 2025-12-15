package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ======================================================
// Gemini HTTP Client (Latency-Tolerant)
// ======================================================

var geminiClient = newGeminiHTTPClient()

func newGeminiHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 90 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}

	return &http.Client{
		Transport: tr,
		Timeout:   90 * time.Second,
	}
}

// ======================================================
// API KEY POOL (Round-Robin, Thread-Safe)
// ======================================================

type GeminiKeyPool struct {
	keys []string
	idx  int
	mu   sync.Mutex
}

func NewGeminiKeyPool(raw string) (*GeminiKeyPool, error) {
	parts := strings.Split(raw, ";")
	keys := make([]string, 0)

	for _, k := range parts {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid gemini api keys provided")
	}

	return &GeminiKeyPool{keys: keys}, nil
}

func (p *GeminiKeyPool) Next() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := p.keys[p.idx]
	p.idx = (p.idx + 1) % len(p.keys)
	return key
}

// ======================================================
// Request / Response Models
// ======================================================

type FactoryRequest struct {
	SystemPromptFactory []string `json:"system_prompt_factory"`
	UserPromptFactory   string   `json:"user_prompt_factory"`

	UserPromptParser   string `json:"user_prompt_parser"`
	SystemPromptParser string `json:"system_prompt_parser"`

	FormText     string `json:"form_text"`
	GeminiAPIKey string `json:"gemini_api_key"` // MULTI KEY ; SEPARATED
	Model        string `json:"model"`
}

type FactoryResponse struct {
	TotalProcessed int      `json:"total_processed"`
	SuccessCount   int      `json:"success_count"`
	Results        []string `json:"results"`
	Errors         []string `json:"errors"`
}

// ======================================================
// Gemini Payload Models
// ======================================================

type GeminiContent struct {
	Role  string `json:"role"`
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

type GeminiPayload struct {
	Contents          []GeminiContent `json:"contents"`
	SystemInstruction *GeminiContent  `json:"system_instruction,omitempty"`
	GenerationConfig  struct {
		Temperature float64 `json:"temperature"`
	} `json:"generation_config"`
}

type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// ======================================================
// HTTP Handler
// ======================================================

func DataFactFactoryHandler(w http.ResponseWriter, r *http.Request) {
	if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	var req FactoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.GeminiAPIKey == "" {
		http.Error(w, "gemini_api_key is required", http.StatusBadRequest)
		return
	}
	if len(req.SystemPromptFactory) == 0 {
		http.Error(w, "system_prompt_factory cannot be empty", http.StatusBadRequest)
		return
	}
	if req.UserPromptFactory == "" || req.UserPromptParser == "" || req.SystemPromptParser == "" {
		http.Error(w, "prompt fields are incomplete", http.StatusBadRequest)
		return
	}

	// 🔒 FORCE MODEL
	req.Model = "gemini-2.5-flash"

	keyPool, err := NewGeminiKeyPool(req.GeminiAPIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)

	n := len(req.SystemPromptFactory)
	results := make([]string, n)

	var wg sync.WaitGroup
	var mu sync.Mutex
	errorsList := []string{}
	success := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		persona := req.SystemPromptFactory[i]

		go func(idx int, personaPrompt string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			out, err := runFactoryThenParse(req, personaPrompt, keyPool)
			if err != nil {
				mu.Lock()
				errorsList = append(errorsList, fmt.Sprintf("Task %d Fail: %v", idx, err))
				mu.Unlock()
				return
			}

			results[idx] = out
			mu.Lock()
			success++
			mu.Unlock()
		}(i, persona)
	}

	wg.Wait()

	resp := FactoryResponse{
		TotalProcessed: n,
		SuccessCount:   success,
		Results:        results,
		Errors:         errorsList,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ======================================================
// Pipeline Logic
// ======================================================

func runFactoryThenParse(req FactoryRequest, persona string, pool *GeminiKeyPool) (string, error) {
	userFactory := req.UserPromptFactory
	if req.FormText != "" {
		userFactory = strings.ReplaceAll(userFactory, "{{ $json.form }}", req.FormText)
	}

	gen, err := callGemini(req.Model, pool.Next(), persona, userFactory)
	if err != nil {
		return "", err
	}

	parserInput := strings.TrimSpace(gen) + "\n\n" + strings.TrimSpace(req.UserPromptParser)

	parsed, err := callGemini(req.Model, pool.Next(), req.SystemPromptParser, parserInput)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(parsed), nil
}

// ======================================================
// Gemini Call (Retry + Timeout, NO FAIL)
// ======================================================

func callGemini(model, apiKey, systemPrompt, userPrompt string) (string, error) {
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		model, apiKey,
	)

	payload := GeminiPayload{
		Contents: []GeminiContent{{
			Role: "user",
			Parts: []struct {
				Text string `json:"text"`
			}{{Text: userPrompt}},
		}},
	}

	if systemPrompt != "" {
		payload.SystemInstruction = &GeminiContent{
			Role: "user",
			Parts: []struct {
				Text string `json:"text"`
			}{{Text: systemPrompt}},
		}
	}

	body, _ := json.Marshal(payload)
	const maxRetry = 4
	var lastErr error

	for i := 0; i <= maxRetry; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := geminiClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("gemini %d: %s", resp.StatusCode, data)
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
			continue
		}

		var gResp GeminiResponse
		if err := json.Unmarshal(data, &gResp); err != nil {
			lastErr = err
			continue
		}

		if len(gResp.Candidates) > 0 && len(gResp.Candidates[0].Content.Parts) > 0 {
			return gResp.Candidates[0].Content.Parts[0].Text, nil
		}
	}

	return "", lastErr
}
