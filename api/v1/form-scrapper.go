package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// --- Models Scrapper ---

type ScrapeRequest struct {
	FormURL string `json:"form_url"`
}

type QuestionItem struct {
	ID      int64    `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

type FormSaveState struct {
	FormID      string  `json:"form_id"`
	Fbzx        string  `json:"fbzx"`
	PageHistory string  `json:"page_history"`
	EntryIDs    []int64 `json:"entry_ids"`
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

	// Menggunakan fastClient dari utils.go
	resp, err := fastClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	content := string(bodyBytes)

	// 1. Regex cari variable FB_PUBLIC_LOAD_DATA_
	re := regexp.MustCompile(`var FB_PUBLIC_LOAD_DATA_ = (.*?);`)
	match := re.FindStringSubmatch(content)
	if len(match) < 2 {
		return nil, fmt.Errorf("gagal menemukan data form (FB_PUBLIC_LOAD_DATA_)")
	}

	jsonStr := match[1]
	
	var rawData []interface{}
	if err := json.Unmarshal([]byte(jsonStr), &rawData); err != nil {
		return nil, fmt.Errorf("gagal parsing JSON form structure")
	}

	// 2. Cari Token FBZX
	var fbzx string
	reFbzx := regexp.MustCompile(`name="fbzx" value="(.*?)"`)
	fbzxMatch := reFbzx.FindStringSubmatch(content)
	if len(fbzxMatch) > 1 {
		fbzx = fbzxMatch[1]
	} else {
		if len(rawData) > 14 {
			if val, ok := rawData[14].(string); ok {
				fbzx = val
			}
		}
	}

	// 3. Parsing Pertanyaan
	if len(rawData) < 2 { return nil, fmt.Errorf("struktur JSON invalid") }
	
	lvl1, ok := rawData[1].([]interface{})
	if !ok || len(lvl1) < 2 { return nil, fmt.Errorf("gagal akses level 1") }
	
	rawQuestions, ok := lvl1[1].([]interface{})
	if !ok { return nil, fmt.Errorf("gagal akses list pertanyaan") }

	var questions []QuestionItem
	var entryIDs []int64

	for _, item := range rawQuestions {
		qArray, ok := item.([]interface{})
		if !ok || len(qArray) < 5 { continue }

		inputDetails, ok := qArray[4].([]interface{})
		if !ok || len(inputDetails) == 0 {
			continue 
		}

		detailInner, ok := inputDetails[0].([]interface{})
		if !ok || len(detailInner) == 0 { continue }
		
		idFloat, ok := detailInner[0].(float64)
		if !ok { continue }
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
	}

	desc, _ := lvl1[0].(string)

	return &ScrapeResponse{
		Description: desc,
		Questions:   questions,
		Saves: FormSaveState{
			FormID:      "scraped_" + strconv.FormatInt(time.Now().Unix(), 10),
			Fbzx:        fbzx,
			PageHistory: "0,1,2,3,4",
			EntryIDs:    entryIDs,
		},
	}, nil
}

// Handler Entry Point
func ScrapperHandler(w http.ResponseWriter, r *http.Request) {
	// Menggunakan mustAuthorize dari utils.go
	if err := mustAuthorize(r); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
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