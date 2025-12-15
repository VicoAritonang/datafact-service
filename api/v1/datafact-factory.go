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

// --- Models Factory ---
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
		Timeout:   75 * time.Second, // PENTING
	}
}

type FactoryRequest struct {
	SystemPromptFactory []string `json:"system_prompt_factory"`
	UserPromptFactory   string   `json:"user_prompt_factory"`

	// Parser stage
	UserPromptParser   string `json:"user_prompt_parser"`
	SystemPromptParser string `json:"system_prompt_parser"`

	// Optional (masih diterima agar kompatibel dengan client lama)
	FormText      string `json:"form_text"`
	GeminiAPIKey  string `json:"gemini_api_key"`
	SpreadsheetID string `json:"spreadsheet_id"` // DIABAIKAN (tidak dipakai lagi)
	Model         string `json:"model"`
}

type FactoryResponse struct {
	TotalProcessed int      `json:"total_processed"`
	SuccessCount   int      `json:"success_count"`
	Results        []string `json:"results"` // hasil PARSER, panjang = N (index selaras persona)
	Errors         []string `json:"errors"`  // daftar error per task
}

// Struct Gemini

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

// --- Handler ---

func DataFactFactoryHandler(w http.ResponseWriter, r *http.Request) {
	// 1) Authorization
	if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	// 2) Parse request
	var req FactoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 3) Validasi minimum
	if req.GeminiAPIKey == "" {
		http.Error(w, "gemini_api_key is required", http.StatusBadRequest)
		return
	}
	if len(req.SystemPromptFactory) == 0 {
		http.Error(w, "system_prompt_factory must be a non-empty array", http.StatusBadRequest)
		return
	}
	if req.UserPromptFactory == "" {
		http.Error(w, "user_prompt_factory is required", http.StatusBadRequest)
		return
	}
	if req.SystemPromptParser == "" {
		http.Error(w, "system_prompt_parser is required", http.StatusBadRequest)
		return
	}
	if req.UserPromptParser == "" {
		http.Error(w, "user_prompt_parser is required", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "gemini-2.5-flash"
	}

	// 4) Concurrency controls
	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)

	// “Buffer” sederhana: rate limit agar tidak burst ke Gemini (stabilin concurrency).
	// Misal: 10 request/detik total. Silakan adjust.
	limiter := time.NewTicker(100 * time.Millisecond) // 10 rps
	defer limiter.Stop()

	// Hasil final: harus berurutan sesuai index persona
	n := len(req.SystemPromptFactory)
	results := make([]string, n)

	var (
		wg           sync.WaitGroup
		muErr        sync.Mutex
		errorsList   []string
		successCount int
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		personaPrompt := req.SystemPromptFactory[i]

		go func(idx int, persona string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			parsed, err := runFactoryThenParse(req, persona, limiter)
			if err != nil {
				muErr.Lock()
				errorsList = append(errorsList, fmt.Sprintf("Task %d Fail: %v", idx, err))
				muErr.Unlock()
				return
			}

			// aman: tiap goroutine menulis index unik
			results[idx] = parsed

			muErr.Lock()
			successCount++
			muErr.Unlock()
		}(i, personaPrompt)
	}

	wg.Wait()

	respData := FactoryResponse{
		TotalProcessed: n,
		SuccessCount:   successCount,
		Results:        results,
		Errors:         errorsList,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(respData)
}

// =====================
// Subroutine: 2-stage pipeline
// =====================

func runFactoryThenParse(req FactoryRequest, persona string, limiter *time.Ticker) (string, error) {
	// Stage A: Factory call
	factoryUser := req.UserPromptFactory
	if req.FormText != "" {
		// kompatibel dengan placeholder n8n Anda
		factoryUser = strings.ReplaceAll(factoryUser, "{{ $json.form }}", req.FormText)
	}

	
	genResult, err := callGemini(req.Model, req.GeminiAPIKey, persona, factoryUser)
	if err != nil {
		return "", fmt.Errorf("factory gemini call failed: %w", err)
	}

	// Stage B: Parser call
	// Anda minta: "{{OUTPUT DARI FACTORY}} + make sure you make it only with this format ..."
	// Jadi kita prepend output factory ke user_prompt_parser.
	parserUser := strings.TrimSpace(genResult) + "\n\n" + strings.TrimSpace(req.UserPromptParser)

	
	parsedRaw, err := callGemini(req.Model, req.GeminiAPIKey, req.SystemPromptParser, parserUser)
	if err != nil {
		return "", fmt.Errorf("parser gemini call failed: %w", err)
	}

	// Output parser dikembalikan “utuh”
	return strings.TrimSpace(parsedRaw), nil
}

// =====================
// Gemini call + helpers
// =====================
func callGemini(model, apiKey, systemPrompt, userPrompt string) (string, error) {
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		model, apiKey,
	)

	payload := GeminiPayload{
		Contents: []GeminiContent{
			{
				Role: "user",
				Parts: []struct {
					Text string `json:"text"`
				}{{Text: userPrompt}},
			},
		},
		GenerationConfig: struct {
			Temperature float64 `json:"temperature"`
		}{Temperature: 0.7},
	}

	if systemPrompt != "" {
		payload.SystemInstruction = &GeminiContent{
			Role: "user",
			Parts: []struct {
				Text string `json:"text"`
			}{{Text: systemPrompt}},
		}
	}

	jsonBody, _ := json.Marshal(payload)

	const maxRetry = 4 // ⬅️ NAIKKAN
	var lastErr error

	for attempt := 0; attempt <= maxRetry; attempt++ {

		// 🔐 context hidup FULL selama request + read body
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

		req, _ := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			url,
			strings.NewReader(string(jsonBody)),
		)
		req.Header.Set("Content-Type", "application/json")

		resp, err := geminiClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		// ⬇️ BODY DIBACA DULU
		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel() // ✅ CANCEL SETELAH BODY SELESAI

		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("gemini api %d: %s", resp.StatusCode, bodyBytes)
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}

		if resp.StatusCode != 200 {
			return "", fmt.Errorf("gemini api error %d: %s", resp.StatusCode, bodyBytes)
		}

		var gResp GeminiResponse
		if err := json.Unmarshal(bodyBytes, &gResp); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		if len(gResp.Candidates) > 0 &&
			len(gResp.Candidates[0].Content.Parts) > 0 {
			return gResp.Candidates[0].Content.Parts[0].Text, nil
		}

		lastErr = fmt.Errorf("empty response")
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}

	return "", fmt.Errorf("gemini failed after retries: %w", lastErr)
}



func cleanMarkdownJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}
