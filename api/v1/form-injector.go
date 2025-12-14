package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort" // Tambahan import untuk sorting
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Models Injector ---

// Kita gunakan json.RawMessage untuk menangani input yang dinamis (bisa string, bisa object)
type InjectRequest struct {
	FormURL string          `json:"form_url"`
	Saves   json.RawMessage `json:"saves"`   // Raw: bisa string JSON atau objek JSON
	Answers json.RawMessage `json:"answers"` // Raw: bisa string JSON atau array JSON
}

type InjectResult struct {
	Total   int      `json:"total"`
	Success int      `json:"success"`
	Failed  int      `json:"failed"`
	Details []string `json:"details"`
}

// --- Helper Functions ---

// helper untuk menangani "Double JSON Encoding" dari n8n
func parseFlexibleJSON(raw json.RawMessage, target interface{}) error {
	var jsonString string
	if err := json.Unmarshal(raw, &jsonString); err == nil {
		return json.Unmarshal([]byte(jsonString), target)
	}
	return json.Unmarshal(raw, target)
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

	// 1. Decode Level 1 (Wrapper)
	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 2. Parsing Flexible 'Saves'
	var savesData FormSaveState
	if len(req.Saves) > 0 {
		if err := parseFlexibleJSON(req.Saves, &savesData); err != nil {
			http.Error(w, "invalid saves format: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 3. Parsing Flexible 'Answers'
	var rawAnswers []interface{}
	if len(req.Answers) > 0 {
		if err := parseFlexibleJSON(req.Answers, &rawAnswers); err != nil {
			http.Error(w, "invalid answers format: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 4. Normalisasi Jawaban (FIXED: Deterministic Ordering)
	var finalAnswers [][]interface{}

	for _, item := range rawAnswers {
		switch v := item.(type) {
		case []interface{}:
			// Format Array: [ "Budi", 20 ] -> Urutan sesuai index (Aman)
			finalAnswers = append(finalAnswers, v)

		case map[string]interface{}:
			// Format Object: { "Nama": "Budi", "Usia": 20 }
			// PERBAIKAN: Menghindari random iteration map di Go.

			// Langkah A: Cek apakah key-nya adalah Entry ID?
			// Ini fitur advanced: user bisa kirim {"12345": "Jawaban"} agar pasti tepat sasaran
			rowByID := make([]interface{}, len(savesData.EntryIDs))
			matchByID := false

			for idx, id := range savesData.EntryIDs {
				idStr := strconv.FormatInt(id, 10)
				// Cek apakah ada key yang sama dengan ID Entry
				if val, ok := v[idStr]; ok {
					rowByID[idx] = val
					matchByID = true
				} else {
					// Jika pakai mode ID tapi data kosong, isi nil
					rowByID[idx] = nil 
				}
			}

			if matchByID {
				// Jika ditemukan setidaknya satu key yang cocok dengan ID, gunakan mode ini
				finalAnswers = append(finalAnswers, rowByID)
			} else {
				// Langkah B: Jika key bukan ID (misal "Nama", "Email")
				// Kita HARUS mengurutkan key secara alfabetis agar stabil (Deterministik)
				// User disarankan memberi prefix di n8n: "1_Nama", "2_Email" agar urut
				var keys []string
				for k := range v {
					keys = append(keys, k)
				}
				sort.Strings(keys) // SORTING DILAKUKAN DISINI

				var row []interface{}
				for _, k := range keys {
					row = append(row, v[k])
				}
				finalAnswers = append(finalAnswers, row)
			}

		default:
			continue
		}
	}

	// Validasi Data
	if len(finalAnswers) == 0 {
		http.Error(w, "no answers provided/parsed", http.StatusBadRequest)
		return
	}
	if len(savesData.EntryIDs) == 0 {
		http.Error(w, "invalid saves data: entry_ids missing", http.StatusBadRequest)
		return
	}

	// 5. Proses Concurrent Injection (TIDAK BERUBAH)
	var wg sync.WaitGroup
	total := len(finalAnswers)
	resultChan := make(chan string, total)

	successCount := 0
	failCount := 0
	var mu sync.Mutex

	maxConcurrency := 20
	semaphore := make(chan struct{}, maxConcurrency)

	for i, ansRow := range finalAnswers {
		wg.Add(1)

		go func(idx int, answerSet []interface{}) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			var responses []interface{}

			// Mapping jawaban ke Entry ID
			for j, val := range answerSet {
				if j >= len(savesData.EntryIDs) {
					break
				}
				
				// Skip jika jawaban nil (untuk partial update via map ID)
				if val == nil {
					continue
				}

				entryID := savesData.EntryIDs[j]
				valStr := fmt.Sprintf("%v", val)

				entryData := []interface{}{
					nil,
					entryID,
					[]string{valStr},
					0,
				}
				responses = append(responses, entryData)
			}

			fullStructure := []interface{}{
				responses,
				nil,
				savesData.Fbzx,
			}

			partialJSON, _ := json.Marshal(fullStructure)

			data := url.Values{}
			data.Set("fvv", "1")
			data.Set("partialResponse", string(partialJSON))
			data.Set("pageHistory", savesData.PageHistory)
			data.Set("fbzx", savesData.Fbzx)
			data.Set("submissionTimestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

			postReq, _ := http.NewRequest("POST", req.FormURL, strings.NewReader(data.Encode()))
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("User-Agent", "Mozilla/5.0 (DataFact Injector Bot)")
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
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}

		}(i, ansRow)
	}

	wg.Wait()
	close(resultChan)

	var details []string
	for msg := range resultChan {
		details = append(details, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(InjectResult{
		Total:   total,
		Success: successCount,
		Failed:  failCount,
		Details: details,
	})
}