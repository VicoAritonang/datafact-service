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

	// 4. Normalisasi Jawaban (Map [ID] -> [Jawaban]) + Support Email
	// Kita buat struktur struct sementara untuk menampung data baris
	type RowData struct {
		AnswersMap map[int64]interface{}
		Email      string
	}
	
	var finalRows []RowData

	for _, item := range rawAnswers {
		rowMap := make(map[int64]interface{})
		var emailAddr string

		switch v := item.(type) {
		case []interface{}:
			// Legacy Array Mode
			for i, val := range v {
				if i < len(savesData.EntryIDs) {
					rowMap[savesData.EntryIDs[i]] = val
				}
			}
			finalRows = append(finalRows, RowData{AnswersMap: rowMap})

		case map[string]interface{}:
			// Object Mode
			for key, val := range v {
				// Cek Khusus Email
				if strings.ToLower(key) == "email" || strings.ToLower(key) == "email address" {
					if eStr, ok := val.(string); ok {
						emailAddr = eStr
					}
					continue
				}

				// 1. Cek Mapping Nama Pertanyaan -> ID
				if id, found := savesData.EntryMappings[key]; found {
					rowMap[id] = val
					continue
				}

				// 2. Cek ID Manual
				if idParsed, err := strconv.ParseInt(key, 10, 64); err == nil {
					// Validasi keberadaan ID
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
			
			if len(rowMap) > 0 || emailAddr != "" {
				finalRows = append(finalRows, RowData{AnswersMap: rowMap, Email: emailAddr})
			}

		default:
			continue
		}
	}

	if len(finalRows) == 0 {
		http.Error(w, "no answers provided/parsed", http.StatusBadRequest)
		return
	}

	// 5. Proses Concurrent Injection
	var wg sync.WaitGroup
	total := len(finalRows)
	resultChan := make(chan string, total)

	successCount := 0
	failCount := 0
	var mu sync.Mutex

	maxConcurrency := 10 // Jangan terlalu agresif ke Google
	semaphore := make(chan struct{}, maxConcurrency)

	for i, row := range finalRows {
		wg.Add(1)

		go func(idx int, rData RowData) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			var responses []interface{}

			// Build payload
			for entryID, val := range rData.AnswersMap {
				if val == nil {
					continue
				}

				// FIX: Handling Slice/Array untuk Checkbox
				var finalVal []string

				switch rawVal := val.(type) {
				case []interface{}:
					// Jika input JSON adalah array: ["A", "B"]
					for _, subVal := range rawVal {
						finalVal = append(finalVal, fmt.Sprintf("%v", subVal))
					}
				case []string:
					finalVal = rawVal
				default:
					// Single value
					finalVal = []string{fmt.Sprintf("%v", val)}
				}
                
                // Jika kosong, skip
                if len(finalVal) == 0 {
                    continue
                }

				// Struktur Entry Google Form: [nil, ID, [Values...], 0]
				entryData := []interface{}{
					nil,
					entryID,
					finalVal, // Harus array of string
					0,
				}
				responses = append(responses, entryData)
			}
            
            // Handle Email (jika ada form yang mewajibkan collect email)
            var emailField interface{} = nil
            if rData.Email != "" {
                emailField = rData.Email
            }

			// Struktur Utama Payload
			fullStructure := []interface{}{
				responses,
				emailField, // Index 1: Email Address (jika di-enable di form)
				savesData.Fbzx,
			}

			partialJSON, _ := json.Marshal(fullStructure)

			data := url.Values{}
			data.Set("fvv", "1")
			data.Set("partialResponse", string(partialJSON))
			data.Set("pageHistory", savesData.PageHistory) // Menggunakan hasil dinamis dari scrapper
			data.Set("fbzx", savesData.Fbzx)
			data.Set("submissionTimestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

			postReq, _ := http.NewRequest("POST", req.FormURL, strings.NewReader(data.Encode()))
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("User-Agent", "Mozilla/5.0 (DataFact Injector Bot)")
			
			// Tambahkan Referer/Origin agar lebih dipercaya
			postReq.Header.Set("Origin", "https://docs.google.com")
			postReq.Header.Set("Referer", req.FormURL)

			resp, err := fastClient.Do(postReq)

			mu.Lock()
			if err == nil && resp.StatusCode == 200 {
				successCount++
                // Optional: Debug success
                // resultChan <- fmt.Sprintf("Row %d success", idx)
			} else {
				failCount++
				errMsg := "unknown error"
				if err != nil {
					errMsg = err.Error()
				} else {
                    // Baca body error google untuk detail
                    bodyErr, _ := io.ReadAll(resp.Body)
                    // Ambil potongan body untuk log (kadang HTML panjang)
                    snippet := string(bodyErr)
                    if len(snippet) > 200 {
                        snippet = snippet[:200]
                    }
					errMsg = fmt.Sprintf("HTTP %d | Body: %s", resp.StatusCode, snippet)
				}
				resultChan <- fmt.Sprintf("Row %d failed: %s", idx, errMsg)
			}
			mu.Unlock()

			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}

		}(i, row)
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