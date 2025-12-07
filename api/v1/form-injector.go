package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Models Injector ---

type InjectRequest struct {
	FormURL string          `json:"form_url"` // Link Response (biasanya diakhiri /formResponse)
	Saves   FormSaveState   `json:"saves"`    // Data 'Saves' yang didapat dari scrapper
	Answers [][]interface{} `json:"answers"`  // Array of Array jawaban
}

type InjectResult struct {
	Total   int      `json:"total"`
	Success int      `json:"success"`
	Failed  int      `json:"failed"`
	Details []string `json:"details"`
}

// --- Handler ---

func InjectorHandler(w http.ResponseWriter, r *http.Request) {
	if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if len(req.Answers) == 0 {
		http.Error(w, "no answers provided", http.StatusBadRequest)
		return
	}

	// Validasi sederhana
	if len(req.Saves.EntryIDs) == 0 {
		http.Error(w, "invalid saves data: entry_ids missing", http.StatusBadRequest)
		return
	}

	// Konfigurasi Concurrency
	var wg sync.WaitGroup
	total := len(req.Answers)
	resultChan := make(chan string, total)
	
	successCount := 0
	failCount := 0
	var mu sync.Mutex // Mutex untuk hitung counter dengan aman

	// Semaphore pattern: Batasi max 20 request bersamaan agar server tidak overload/banned
	maxConcurrency := 20
	semaphore := make(chan struct{}, maxConcurrency)

	// Loop setiap set jawaban
	for i, ansRow := range req.Answers {
		wg.Add(1)
		
		go func(idx int, answerSet []interface{}) {
			defer wg.Done()
			
			// Acquire token (tunggu jika penuh)
			semaphore <- struct{}{}
			defer func() { <-semaphore }() // Release token

			// 1. Mapping Jawaban ke Entry ID
			// Format Google Form: [None, EntryID, [JawabanString], 0]
			var responses []interface{}

			for j, val := range answerSet {
				// Cegah index out of range jika jawaban lebih banyak dari pertanyaan
				if j >= len(req.Saves.EntryIDs) { break }
				
				entryID := req.Saves.EntryIDs[j]
				valStr := fmt.Sprintf("%v", val) // Konversi apapun ke string
				
				entryData := []interface{}{
					nil, 
					entryID, 
					[]string{valStr}, 
					0,
				}
				responses = append(responses, entryData)
			}

			// 2. Bungkus ke struktur final JSON
			// [responses_array, null, fbzx_token]
			fullStructure := []interface{}{
				responses,
				nil,
				req.Saves.Fbzx,
			}
			
			partialJSON, _ := json.Marshal(fullStructure)

			// 3. Buat Payload POST Form
			data := url.Values{}
			data.Set("fvv", "1")
			data.Set("partialResponse", string(partialJSON))
			data.Set("pageHistory", req.Saves.PageHistory)
			data.Set("fbzx", req.Saves.Fbzx)
			data.Set("submissionTimestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

			// 4. Kirim Request
			postReq, _ := http.NewRequest("POST", req.FormURL, strings.NewReader(data.Encode()))
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("User-Agent", "Mozilla/5.0 (DataFact Injector Bot)")
			// Header Origin/Referer kadang dibutuhkan
			postReq.Header.Set("Origin", "https://docs.google.com")
			
			resp, err := fastClient.Do(postReq)
			
			mu.Lock()
			if err == nil && resp.StatusCode == 200 {
				successCount++
			} else {
				failCount++
				errMsg := "unknown error"
				if err != nil {
					errMsg = err.Error()
				} else {
					errMsg = fmt.Sprintf("HTTP Status %d", resp.StatusCode)
				}
				resultChan <- fmt.Sprintf("Row %d failed: %s", idx, errMsg)
			}
			mu.Unlock()
			
			if resp != nil {
				io.Copy(io.Discard, resp.Body) // Kuras body agar koneksi reuse
				resp.Body.Close()
			}

		}(i, ansRow)
	}

	// Tunggu semua selesai
	wg.Wait()
	close(resultChan)

	// Kumpulkan log error
	var details []string
	for msg := range resultChan {
		details = append(details, msg)
	}

	// Response Final
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(InjectResult{
		Total:   total,
		Success: successCount,
		Failed:  failCount,
		Details: details,
	})
}