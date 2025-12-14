package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// --- Models Factory ---

type FactoryRequest struct {
	SystemPromptFactory []string `json:"system_prompt_factory"`
	UserPromptFactory   string   `json:"user_prompt_factory"`
	UserPromptParser    string   `json:"user_prompt_parser"`
	SystemPromptParser  string   `json:"system_prompt_parser"`
	FormText            string   `json:"form_text"`
	GeminiAPIKey        string   `json:"gemini_api_key"`
	SpreadsheetID       string   `json:"spreadsheet_id"`
	Model               string   `json:"model"`
}

type FactoryResponse struct {
	TotalProcessed int      `json:"total_processed"`
	SuccessCount   int      `json:"success_count"`
	Errors         []string `json:"errors"`
}

// ... (Struct GeminiContent, GeminiPayload, GeminiResponse SAMA SEPERTI SEBELUMNYA) ...
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
	// 1. Authorize
	if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	// 2. Decode Body
	var req FactoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validasi
	if req.SpreadsheetID == "" || req.GeminiAPIKey == "" {
		http.Error(w, "spreadsheet_id and gemini_api_key are required", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "gemini-2.5-flash"
	}

	// 3. Init Google Sheets
	ctx := context.Background()
	sheetsService, err := createSheetsServiceWithRefreshToken(ctx)
	if err != nil {
		fmt.Printf("Sheets Auth Error: %v\n", err)
		http.Error(w, "failed to init sheets service: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 4. Ambil Header Spreadsheet (FIX: Explicit Sheet1)
	// Kita baca baris pertama Sheet1 untuk mendapatkan header
	readRange := "Sheet1!A1:Z1"
	resp, err := sheetsService.Spreadsheets.Values.Get(req.SpreadsheetID, readRange).Do()
	if err != nil {
		http.Error(w, "failed to read spreadsheet header: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if len(resp.Values) == 0 {
		http.Error(w, "Sheet1 is empty, please set headers in row 1", http.StatusBadRequest)
		return
	}

	// Mapping Header Name -> Index Column (FIX: Case Insensitive)
	// Kita simpan header dalam lowercase agar pencocokan lebih robust
	headerMap := make(map[string]int)
	headers := resp.Values[0]
	for i, col := range headers {
		if colStr, ok := col.(string); ok {
			cleanName := strings.ToLower(strings.TrimSpace(colStr))
			headerMap[cleanName] = i
		}
	}

	// 5. Orchestration Loop
	var wg sync.WaitGroup
	var mu sync.Mutex

	maxConcurrency := 5
	sem := make(chan struct{}, maxConcurrency)

	resultDetails := []string{}
	successCount := 0

	totalTasks := len(req.SystemPromptFactory)

	for i, personaPrompt := range req.SystemPromptFactory {
		wg.Add(1)

		go func(idx int, persona string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			// --- PHASE 1: GENERATE (Persona) ---
			taskUserPrompt := req.UserPromptFactory
			if req.FormText != "" {
				taskUserPrompt = strings.ReplaceAll(taskUserPrompt, "{{ $json.form }}", req.FormText)
			}

			genResult, err := callGemini(req.Model, req.GeminiAPIKey, persona, taskUserPrompt)
			if err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Gen Error: %v", idx, err))
				mu.Unlock()
				return
			}

			// --- PHASE 2: PARSER ---
			parserUserPrompt := req.UserPromptParser
			n8nPlaceholder := "{{ $json.choices[0].message.content }}"
			
			if strings.Contains(parserUserPrompt, n8nPlaceholder) {
				parserUserPrompt = strings.ReplaceAll(parserUserPrompt, n8nPlaceholder, genResult)
			} else {
				parserUserPrompt = parserUserPrompt + "\n\nInput Text:\n" + genResult
			}

			parsedRawStr, err := callGemini(req.Model, req.GeminiAPIKey, req.SystemPromptParser, parserUserPrompt)
			if err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Parse Error: %v", idx, err))
				mu.Unlock()
				return
			}

			// --- PHASE 3: CLEAN & DECODE JSON ---
			cleanedJSON := cleanMarkdownJSON(parsedRawStr)
			
			var dataMap map[string]interface{}
			if err := json.Unmarshal([]byte(cleanedJSON), &dataMap); err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d JSON Error: %v | Raw: %s", idx, err, cleanedJSON))
				mu.Unlock()
				return
			}

			// --- PHASE 4: PREPARE ROW DATA (AUTO MAP) ---
			
			// Buat array kosong seukuran jumlah header
			rowValues := make([]interface{}, len(headers))
			// Isi default string kosong
			for k := range rowValues {
				rowValues[k] = ""
			}

			// Smart Mapping: Loop data dari JSON, cari posisi kolomnya di HeaderMap
			hasData := false
			for key, val := range dataMap {
				// Ubah key JSON ke lowercase & trim space agar cocok dengan header
				keyClean := strings.ToLower(strings.TrimSpace(key))
				
				if colIdx, found := headerMap[keyClean]; found {
					rowValues[colIdx] = val
					hasData = true
				}
			}

			// DEBUG LOG: Cek apakah rowValues terisi
			if !hasData {
				fmt.Printf("Warning Task %d: No matching headers found. JSON keys: %v\n", idx, dataMap)
			}

			// --- PHASE 5: WRITE TO SHEET ---
			// FIX: Gunakan "Sheet1!A1" agar append pasti masuk ke Sheet1
			vr := &sheets.ValueRange{
				Values: [][]interface{}{rowValues},
			}
			
			_, err = sheetsService.Spreadsheets.Values.Append(req.SpreadsheetID, "Sheet1!A1", vr).ValueInputOption("USER_ENTERED").Do()
			if err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Sheet Write Error: %v", idx, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			successCount++
			mu.Unlock()

		}(i, personaPrompt)
	}

	wg.Wait()

	respData := FactoryResponse{
		TotalProcessed: totalTasks,
		SuccessCount:   successCount,
		Errors:         resultDetails,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(respData)
}

// ... (Helper Functions createSheetsServiceWithRefreshToken, callGemini, cleanMarkdownJSON SAMA PERSIS) ...
// Pastikan helper functions tetap ada di file ini atau di utils.go
// Saya sertakan lagi di bawah agar copy-paste aman

func createSheetsServiceWithRefreshToken(ctx context.Context) (*sheets.Service, error) {
	refreshToken := os.Getenv("DATAFACT_GOOGLE_REFRESH_TOKEN")
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")

	if refreshToken == "" || clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("missing google oauth env vars")
	}

	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{sheets.SpreadsheetsScope},
	}

	token := &oauth2.Token{
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
	}

	client := config.Client(ctx, token)
	return sheets.NewService(ctx, option.WithHTTPClient(client))
}

func callGemini(model, apiKey, systemPrompt, userPrompt string) (string, error) {
	// [Kode callGemini sama seperti sebelumnya, tidak ada perubahan logika]
    // ... Implementasi sama ...
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)

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
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(jsonBody)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := fastClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini api error %d: %s", resp.StatusCode, string(body))
	}

	var gResp GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return "", err
	}

	if len(gResp.Candidates) > 0 && len(gResp.Candidates[0].Content.Parts) > 0 {
		return gResp.Candidates[0].Content.Parts[0].Text, nil
	}

	return "", fmt.Errorf("no content generated")
}

func cleanMarkdownJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}