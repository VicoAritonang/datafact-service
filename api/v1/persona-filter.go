package handler

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// =====================
// Config & Structs
// =====================

type FilterBody struct {
	// filter bisa berupa object JSON ATAU string JSON. Kita normalize di handler.
	Filter json.RawMessage `json:"filter"`
	Limit  *int            `json:"limit,omitempty"`
	Offset *int            `json:"offset,omitempty"`
}

type SupabaseConfig struct {
	BaseURL string
	APIKey  string
	Schema  string
	Table   string
}

func loadSupabaseConfig() SupabaseConfig {
	// Fungsi mustenv dan getenv diambil dari utils.go
	return SupabaseConfig{
		BaseURL: mustenv("SUPABASE_URL"),
		APIKey:  mustenv("SUPABASE_SERVICE_ROLE_KEY"),
		Schema:  getenv("SUPABASE_DB_SCHEMA", "public"),
		Table:   getenv("SUPABASE_PERSONA_TABLE", "persona_bank"),
	}
}

// =====================
// Kolom Mapping
// =====================

var (
	numericCols = map[string]struct{}{
		"usia": {}, "jumlah_anak": {}, "penghasilan_bulanan": {},
		"quality_score": {}, "slang_level": {}, "tingkat_ekstrovert": {},
		"tingkat_kepercayaan_opini": {}, "usage_count": {},
	}
	booleanCols = map[string]struct{}{
		"is_active": {}, "eligible_basic": {}, "eligible_pro": {},
	}
	stringCols = map[string]struct{}{
		// identitas & lokasi
		"nama": {}, "jenis_kelamin": {}, "status_pernikahan": {},
		"domisili_provinsi": {}, "domisili_kota": {}, "tipe_tinggal": {},
		// pendidikan & pekerjaan
		"pendidikan_terakhir": {}, "jurusan_pendidikan": {}, "pekerjaan": {},
		"industri_pekerjaan": {}, "status_pekerjaan": {},
		// preferensi & gaya hidup
		"aktivitas_harian": {}, "gaya_hidup": {}, "kebiasaan_belanja": {},
		// digital & kepribadian
		"jam_online_utama": {}, "tech_comfort_level": {}, "kepribadian_mbti": {},
		"gaya_komunikasi": {}, "nada_jawaban_default": {},
		"bahasa_utama": {}, "panjang_jawaban_preferensi": {},
	}
)

func baseNumericField(field string) (base, bound string, ok bool) {
	for col := range numericCols {
		if field == col+"_min" {
			return col, "min", true
		}
		if field == col+"_max" {
			return col, "max", true
		}
	}
	return "", "", false
}

// =====================
// Normalisasi filter
// =====================

func normalizeFilter(raw json.RawMessage) (map[string]interface{}, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]interface{}{}, nil
	}

	// Coba parse sebagai object langsung
	var asMap map[string]interface{}
	if err := json.Unmarshal(raw, &asMap); err == nil {
		return asMap, nil
	}

	// Kalau gagal, coba parse sebagai string yang berisi JSON
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return nil, fmt.Errorf("filter harus object atau string JSON: %w", err)
	}
	asString = strings.TrimSpace(asString)
	if asString == "" {
		return map[string]interface{}{}, nil
	}
	if err := json.Unmarshal([]byte(asString), &asMap); err != nil {
		return nil, fmt.Errorf("string filter bukan JSON valid: %w", err)
	}
	return asMap, nil
}

// =====================
// Query Builder (concurrent)
// =====================

func buildPostgrestQuery(filter map[string]interface{}, fb FilterBody, table string) (string, error) {
	q := url.Values{}
	q.Set("select", "*")

	type bounds struct{ min, max *int64 }
	numBounds := map[string]*bounds{}
	stringIn := map[string][]string{}
	boolEq := map[string]bool{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	// kita tidak akan return error untuk field tak dikenal—sesuai request "abaikan"
	errCh := make(chan error, len(filter))

	for key, val := range filter {
		k, v := strings.TrimSpace(key), val
		if k == "" || v == nil {
			continue
		}
		wg.Add(1)
		go func(field string, value interface{}) {
			defer wg.Done()

			// 1) usia_min / usia_max / <num>_min / <num>_max
			if base, bound, ok := baseNumericField(field); ok {
				// toInt64 dari utils.go
				n, err := toInt64(value)
				if err != nil {
					return
				}
				mu.Lock()
				if _, ok := numBounds[base]; !ok {
					numBounds[base] = &bounds{}
				}
				if bound == "min" {
					numBounds[base].min = &n
				} else {
					numBounds[base].max = &n
				}
				mu.Unlock()
				return
			}

			// 2) boolean kolom langsung
			if _, isBool := booleanCols[field]; isBool {
				// toBool dari utils.go
				b, err := toBool(value)
				if err != nil {
					return
				}
				mu.Lock()
				boolEq[field] = b
				mu.Unlock()
				return
			}

			// 3) string kolom langsung → eq/in
			if _, isStr := stringCols[field]; isStr {
				// toStringSlice dari utils.go
				vals, err := toStringSlice(