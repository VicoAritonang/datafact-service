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
	CookieEmail int            `json:"cookie_email"` // 1 = Cookie/Auto, 0 = Manual/None
}

// --- Logic ---

func scrapeGoogleForm(formURL string) (*ScrapeResponse, error) {
	req, _ := http.NewRequest("GET", formURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := fastClient.Do(req) // Pastikan fastClient sudah didefinisikan di package Anda (var global)
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
		if len(rawData) > 14 {
			if val, ok := rawData[14].(string); ok {
				fbzx = val
			} else if valFloat, ok := rawData[14].(float64); ok {
				fbzx = strconv.FormatFloat(valFloat, 'f', -1, 64)
			}
		}
	}

	// 3. Parsing Pertanyaan, Page History, & Cookie Email
	if len(rawData) < 2 {
		return nil, fmt.Errorf("struktur JSON invalid")
	}

	lvl1, ok := rawData[1].([]interface{})
	if !ok || len(lvl1) < 2 {
		return nil, fmt.Errorf("gagal akses level 1")
	}

	// --- LOGIC BARU: Cek Cookie Email ---
	// Index 10 di lvl1 menentukan tipe koleksi email.
	// 1 = Input Manual (Responder Input) -> cookie_email = 0
	// 2 = Verified (Login Required/Cookie) -> cookie_email = 1
	// 0 atau null = Tidak collect -> cookie_email = 0
	cookieEmail := 0
	if len(lvl1) > 10 {
		if valFloat, ok := lvl1[10].(float64); ok {
			if int(valFloat) == 2 {
				cookieEmail = 1
			}
		}
	}

	rawQuestions, ok := lvl1[1].([]interface{})
	if !ok {
		return nil, fmt.Errorf("gagal akses list pertanyaan")
	}

	var questions []QuestionItem
	var entryIDs []int64
	entryMappings := make(map[string]int64)

	pageCount := 0

	for _, item := range rawQuestions {
		qArray, ok := item.([]interface{})
		if !ok || len(qArray) < 4 {
			continue
		}

		var itemType int
		if tFloat, ok := qArray[3].(float64); ok {
			itemType = int(tFloat)
		}

		if itemType == 8 {
			pageCount++
			continue
		}

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

	var pageHistoryParts []string
	for i := 0; i <= pageCount; i++ {
		pageHistoryParts = append(pageHistoryParts, strconv.Itoa(i))
	}
	finalPageHistory := strings.Join(pageHistoryParts, ",")

	desc, _ := lvl1[0].(string)

	return &ScrapeResponse{
		Description: desc,
		Questions:   questions,
		CookieEmail: cookieEmail, // <-- Field Baru
		Saves: FormSaveState{
			FormID:        "scraped_" + strconv.FormatInt(time.Now().Unix(), 10),
			Fbzx:          fbzx,
			PageHistory:   finalPageHistory,
			EntryIDs:      entryIDs,
			EntryMappings: entryMappings,
		},
	}, nil
}

func ScrapperHandler(w http.ResponseWriter, r *http.Request) {
    // Auth check (jika ada fungsi mustAuthorize)
    // if err := mustAuthorize(r); err != nil { ... }

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