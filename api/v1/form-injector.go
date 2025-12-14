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

// FormSaveState dihapus dari sini, menggunakan yang ada di utils.go

type InjectRequest struct {
	FormURL string          `json:"form_url"`
	Saves   json.RawMessage `json:"saves"`
	Answers json.RawMessage `json:"answers"`
}

type InjectResult struct {
	Total   int      `json:"total"`
	Success int      `json:"success"`
	Failed  int      `json:"failed"`
	Details []string `json:"details"`
}

// --- Helper Functions ---

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

	// 1. Decode Wrapper
	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 2. Parsing Flexible 'Saves' (Menggunakan struct dari utils.go)
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

	// 4. Normalisasi Jawaban (FIX: Menggunakan Mapping, bukan Sorting)
	// Kita ubah struktur penyimpanan menjadi Map [ID] -> [Jawaban]
	var finalAnswers []map[int64]interface{}

	for _, item := range rawAnswers {
		rowMap := make(map[int64]interface{})

		switch v := item.(type) {
		case []interface{}:
			// Format Array: [ "Budi", 20 ] -> Masih mengandalkan urutan index entry_ids (Legacy mode)
			for i, val := range v {
				if i < len(savesData.EntryIDs) {
					rowMap[savesData.EntryIDs[i]] = val
				}
			}
			finalAnswers = append(finalAnswers, rowMap)

		case map[string]interface{}:
			// Format Object: { "Nama": "Budi", "Email": "..." }
			// KITA GUNAKAN MAPPING DARI SCRAPPER
			
			for key, val := range v {
				// 1. Cek apakah key adalah Nama Pertanyaan (via EntryMappings)
				if id, found := savesData.EntryMappings[key]; found {
					rowMap[id] = val
					continue
				}

				// 2. Fallback: Cek jika key itu sendiri adalah ID (user kirim ID manual)
				if idParsed, err := strconv.ParseInt(key, 10, 64); err == nil {
					// Pastikan ID ini valid ada di form
					isValid := false
					for _, eid := range savesData.EntryIDs {
						if eid == idParsed {
							isValid = true
							break
						}
					}
					if isValid {
						rowMap[idParsed] = val
					}
				}
			}
			
			if len(rowMap) > 0 {
				finalAnswers = append(finalAnswers, rowMap)
			}

		default:
			continue
		}
	}

	// Validasi Data
	if len(finalAnswers) == 0 {
		http.Error(w, "no answers provided/parsed or mapping failed", http.StatusBadRequest)
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

	for i, ansRowMap := range finalAnswers {
		wg.Add(1)

		go func(idx int, answerMap map[int64]interface{}) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			var responses []interface{}

			// Build payload dari Map ID -> Jawaban
			for entryID, val := range answerMap {
				if val == nil {
					continue
				}
				valStr := fmt.Sprintf("%v", val)

				// Struktur Google Form Entry
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

		}(i, ansRowMap)
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