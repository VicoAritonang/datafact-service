package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// --- Models Scrapper (Tetap sama) ---
type ScrapeRequest struct {
	FormURL string `json:"form_url"`
}

type QuestionItem struct {
	ID      int64    `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

type ScrapeResponse struct {
	Description string        `json:"description"`
	Questions   []QuestionItem `json:"questions"`
	Saves       FormSaveState `json:"saves"`
}

// --- Logic ---

func scrapeGoogleForm(formURL string) (*ScrapeResponse, error) {
	req, _ := http.NewRequest("GET", formURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := fastClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	content := string(bodyBytes)

	// 1. Ekstrak FB_PUBLIC_LOAD_DATA_
	re := regexp.MustCompile(`var\s+FB_PUBLIC_LOAD_DATA_\s*=\s*([\s\S]*?);\s*</script>`)
	match := re.FindStringSubmatch(content)

	if len(match) < 2 {
		reFallback := regexp.MustCompile(`var\s+FB_PUBLIC_LOAD_DATA_\s*=\s*(\[[\s\S]*\]);`)
		match = reFallback.FindStringSubmatch(content)
	}

	if len(match) < 2 {
		return nil, fmt.Errorf("gagal menemukan data form (FB_PUBLIC_LOAD_DATA_)")
	}

	jsonStr := match[1]
	var rawData []interface{}
	if err := json.Unmarshal([]byte(jsonStr), &rawData); err != nil {
		return nil, fmt.Errorf("gagal parsing JSON form structure: %v", err)
	}

	// 2. Cari Token FBZX
	var fbzx string
	reFbzx := regexp.MustCompile(`name=["']fbzx["']\s+value=["'](.*?)["']`)
	fbzxMatch := reFbzx.FindStringSubmatch(content)
	if len(fbzxMatch) > 1 {
		fbzx = fbzxMatch[1]
	} else {
		// Fallback cari di rawData index 14
		if len(rawData) > 14 {
			if val, ok := rawData[14].(string); ok {
				fbzx = val
			} else if valFloat, ok := rawData[14].(float64); ok {
				fbzx = strconv.FormatFloat(valFloat, 'f', -1, 64)
			}
		}
	}

	// 3. Parsing Pertanyaan & Page History
	if len(rawData) < 2 {
		return nil, fmt.Errorf("struktur JSON invalid")
	}

	lvl1, ok := rawData[1].([]interface{})
	if !ok || len(lvl1) < 2 {
		return nil, fmt.Errorf("gagal akses level 1")
	}

	rawQuestions, ok := lvl1[1].([]interface{})
	if !ok {
		return nil, fmt.Errorf("gagal akses list pertanyaan")
	}

	var questions []QuestionItem
	var entryIDs []int64
	entryMappings := make(map[string]int64)
	
	// -- LOGIC BARU: Dynamic Page History --
	// Page 0 selalu ada. Setiap ketemu "Type 8" (Section Break), page nambah.
	pageCount := 0
	
	for _, item := range rawQuestions {
		qArray, ok := item.([]interface{})
		if !ok || len(qArray) < 4 {
			continue
		}

		// Cek Tipe Item (Index ke-3)
		// 0=Short, 1=Paragraph, 2=Radio, 4=Checkbox, 8=SectionHeader (PageBreak), dll
		var itemType int
		if tFloat, ok := qArray[3].(float64); ok {
			itemType = int(tFloat)
		}

		// Jika tipe adalah 8, ini adalah Page Break
		if itemType == 8 {
			pageCount++
			continue // Section header bukan pertanyaan input
		}

		// Cek detail input (Index ke-4) untuk pertanyaan biasa
		if len(qArray) < 5 {
			continue
		}
		
		inputDetails, ok := qArray[4].([]interface{})
		if !ok || len(inputDetails) == 0 {
			continue
		}

		detailInner, ok := inputDetails[0].([]interface{})
		if !ok || len(detailInner) == 0 {
			continue
		}

		idFloat, ok := detailInner[0].(float64)
		if !ok {
			continue
		}
		entryID := int64(idFloat)
		qText, _ := qArray[1].(string)

		// Ambil Opsi Jawaban (jika ada)
		var options []string
		if len(detailInner) > 1 {
			if optsRaw, ok := detailInner[1].([]interface{}); ok {
				for _, o := range optsRaw {
					if optArr, ok := o.([]interface{}); ok && len(optArr) > 0 {
						if optStr, ok := optArr[0].(string); ok {
							options = append(options, optStr)
						}
					}
				}
			}
		}

		questions = append(questions, QuestionItem{
			ID:      entryID,
			Text:    qText,
			Options: options,
		})
		entryIDs = append(entryIDs, entryID)

		if qText != "" {
			entryMappings[qText] = entryID
		}
	}

	// Generate String Page History (contoh: "0,1,2")
	var pageHistoryParts []string
	for i := 0; i <= pageCount; i++ {
		pageHistoryParts = append(pageHistoryParts, strconv.Itoa(i))
	}
	finalPageHistory := strings.Join(pageHistoryParts, ",")

	desc, _ := lvl1[0].(string)

	return &ScrapeResponse{
		Description: desc,
		Questions:   questions,
		Saves: FormSaveState{
			FormID:        "scraped_" + strconv.FormatInt(time.Now().Unix(), 10),
			Fbzx:          fbzx,
			PageHistory:   finalPageHistory, // DINAMIS, TIDAK HARDCODED
			EntryIDs:      entryIDs,
			EntryMappings: entryMappings,
		},
	}, nil
}

// ScrapperHandler tetap sama...
func ScrapperHandler(w http.ResponseWriter, r *http.Request) {
    // ... copy logic handler yang lama ...
    // Pastikan memanggil scrapeGoogleForm yang baru
    // ...
    if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	var req ScrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if req.FormURL == "" {
		http.Error(w, "form_url is required", http.StatusBadRequest)
		return
	}

	data, err := scrapeGoogleForm(req.FormURL)
	if err != nil {
		http.Error(w, "scraping failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}