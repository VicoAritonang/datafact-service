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

// Struct Gemini (Sama seperti sebelumnya)
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
	// 1. Authorization
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

	// Validasi
	if req.SpreadsheetID == "" || req.GeminiAPIKey == "" {
		http.Error(w, "spreadsheet_id and gemini_api_key are required", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "gemini-2.5-flash"
	}

	// 2. Init Google Sheets Service
	ctx := context.Background()
	sheetsService, err := createSheetsServiceWithRefreshToken(ctx)
	if err != nil {
		fmt.Printf("Sheets Auth Error: %v\n", err)
		http.Error(w, "failed to init sheets service: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. READ HEADER (Manual Map Automatically Logic)
	// Kita baca baris pertama untuk tahu urutan kolom
	readRange := "Sheet1!A1:ZZ1" // Baca selebar mungkin di baris 1
	resp, err := sheetsService.Spreadsheets.Values.Get(req.SpreadsheetID, readRange).Do()
	if err != nil {
		http.Error(w, "failed to read header: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if len(resp.Values) == 0 {
		http.Error(w, "Sheet1 header row is empty", http.StatusBadRequest)
		return
	}

	headers := resp.Values[0] // Interface slice: ["Nama", "Usia", "Kota"]
	
	// Simpan urutan header: map["nama"] = 0, map["usia"] = 1
	headerIndexMap := make(map[string]int)
	for i, h := range headers {
		if s, ok := h.(string); ok {
			// Lowercase & trim agar matching lebih gampang
			cleanHeader := strings.ToLower(strings.TrimSpace(s))
			headerIndexMap[cleanHeader] = i
		}
	}
	
	// Debug Log untuk memastikan Header terbaca
	fmt.Printf("[DEBUG] Headers Found: %v\n", headerIndexMap)

	// 4. Orchestration Loop
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

			// --- A. Gemini Generate ---
			taskUserPrompt := req.UserPromptFactory
			if req.FormText != "" {
				taskUserPrompt = strings.ReplaceAll(taskUserPrompt, "{{ $json.form }}", req.FormText)
			}
			genResult, err := callGemini(req.Model, req.GeminiAPIKey, persona, taskUserPrompt)
			if err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Gen Fail: %v", idx, err))
				mu.Unlock()
				return
			}

			// --- B. Gemini Parser ---
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
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Parse Fail: %v", idx, err))
				mu.Unlock()
				return
			}

			// --- C. JSON Parsing ---
			cleanedJSON := cleanMarkdownJSON(parsedRawStr)
			var dataMap map[string]interface{}
			if err := json.Unmarshal([]byte(cleanedJSON), &dataMap); err != nil {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d JSON Fail: %v (Raw: %s)", idx, err, cleanedJSON))
				mu.Unlock()
				return
			}

			// --- D. MAPPING (The "Map Automatically" Logic) ---
			// Siapkan array kosong seukuran jumlah header
			rowValues := make([]interface{}, len(headers))
			// Isi default string kosong agar tidak null
			for k := range rowValues {
				rowValues[k] = ""
			}

			mappedCount := 0
			// Loop setiap key dari JSON hasil AI
			for jsonKey, jsonVal := range dataMap {
				// Bersihkan key dari JSON
				cleanKey := strings.ToLower(strings.TrimSpace(jsonKey))
				
				// Cek apakah key ini ada di header sheet kita?
				if colIdx, exists := headerIndexMap[cleanKey]; exists {
					rowValues[colIdx] = jsonVal
					mappedCount++
				} else {
					// Opsional: Log jika ada key dari AI yang tidak ada kolomnya di Sheet
					// fmt.Printf("[DEBUG] Key '%s' from AI not found in Sheet Headers\n", jsonKey)
				}
			}

			// Jika tidak ada satupun data yang ter-mapping, kemungkinan besar format header beda
			if mappedCount == 0 {
				mu.Lock()
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Warning: No columns mapped! AI Keys: %v vs Headers: %v", idx, dataMap, headers))
				mu.Unlock()
				return // Skip write daripada nulis baris kosong
			}

			// --- E. WRITE TO SHEET ---
			
			// ValueRange object
			vr := &sheets.ValueRange{
				Values: [][]interface{}{rowValues},
			}

			// APPEND REQUEST
			// valueInputOption: USER_ENTERED (Penting agar angka masuk sebagai angka)
			// insertDataOption: INSERT_ROWS (Penting agar sheet tidak cuma update row kosong tapi bikin row baru)
			// range: "Sheet1" (Nama sheet saja, biarkan Google tentukan baris paling bawah)
			
			// Kita lock mutex karena write sheet sebaiknya serial untuk menghindari race condition di API
			mu.Lock() 
			_, err = sheetsService.Spreadsheets.Values.Append(req.SpreadsheetID, "Sheet1", vr).
				ValueInputOption("USER_ENTERED").
				InsertDataOption("INSERT_ROWS").
				Do()
				
			if err != nil {
				resultDetails = append(resultDetails, fmt.Sprintf("Task %d Write Fail: %v", idx, err))
			} else {
				successCount++
			}
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

// ==========================================
// HELPER FUNCTIONS (JANGAN LUPA DISERTAKAN)
// ==========================================

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
	// (Kode callGemini sama seperti sebelumnya, copy paste dari jawaban sebelumnya)
    // Pastikan pakai fastClient dari utils.go
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