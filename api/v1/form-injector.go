package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// n8n sering mengirim field sebagai string: "saves": "{\"id\":1}"
// fungsi ini akan membukanya menjadi struct asli
func parseFlexibleJSON(raw json.RawMessage, target interface{}) error {
	// 1. Coba unmarshal sebagai string dulu (kasus n8n)
	var jsonString string
	if err := json.Unmarshal(raw, &jsonString); err == nil {
		// Jika berhasil jadi string, unmarshal isi stringnya ke target
		return json.Unmarshal([]byte(jsonString), target)
	}
	// 2. Jika bukan string, berarti sudah object, unmarshal langsung
	return json.Unmarshal(raw, target)
}

// --- Handler ---

func InjectorHandler(w http.ResponseWriter, r *http.Request) {
	// Menggunakan mustAuthorize dari utils.go
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

	// 2. Parsing Flexible 'Saves' (String atau Object)
	var savesData FormSaveState
	if len(req.Saves) > 0 {
		if err := parseFlexibleJSON(req.Saves, &savesData); err != nil {
			http.Error(w, "invalid saves format: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 3. Parsing Flexible 'Answers' (String atau Array)
	var rawAnswers []interface{}
	if len(req.Answers) > 0 {
		if err := parseFlexibleJSON(req.Answers, &rawAnswers); err != nil {
			http.Error(w, "invalid answers format: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 4. Normalisasi Jawaban (Map ke Slice)
	// Input n8n berbentuk [{"Nama":"Budi", "Usia":20}] -> Map
	// Kita butuh [["Budi", 20]] -> Slice
	var finalAnswers [][]interface{}

	for _, item := range rawAnswers {
		switch v := item.(type) {
		case []interface{}:
			// Sudah format list (Aman)
			finalAnswers = append(finalAnswers, v)
		case map[string]interface{}:
			// Format Object (Bahaya: Order map di Go acak)
			// Kita coba ekstrak values-nya saja. 
			// NOTE: Idealnya n8n mengirim array of array, bukan array of object.
			// Kode ini akan mencoba mengambil value, tapi urutannya mungkin tidak sesuai form
			// karena map[string] tidak punya urutan pasti.
			var row []interface{}
			// PENTING: Karena map tidak berurut, kita pakai entryIDs dari Saves sebagai acuan jumlah
			// Tapi kita tidak punya key mapping (misal ID 123 = "Nama"). 
			// Jadi kita terpaksa mengambil semua value dari map.
			for _, val := range v {
				row = append(row, val)
			}
			finalAnswers = append(finalAnswers, row)
		default:
			// Format tidak dikenal
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

	// 5. Proses Concurrent Injection
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
				if j >= len(savesData.EntryIDs) { break }
				
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

			// Menggunakan fastClient dari utils.go
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