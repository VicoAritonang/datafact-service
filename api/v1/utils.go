package handler

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// =====================
// 1. Shared Models
// =====================

// FormSaveState dipindahkan ke sini agar bisa dipakai oleh scrapper dan injector
// tanpa error "redeclared".
type FormSaveState struct {
	FormID        string           `json:"form_id"`
	Fbzx          string           `json:"fbzx"`
	PageHistory   string           `json:"page_history"`
	EntryIDs      []int64          `json:"entry_ids"`
	// Field Baru: Menyimpan peta "Pertanyaan" -> "ID"
	EntryMappings map[string]int64 `json:"entry_mappings"` 
}

// =====================
// 2. Shared HTTP Client
// =====================

var fastClient = newFastHTTPClient()

func newFastHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 90 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

// =====================
// 3. Authentication
// =====================

func mustAuthorize(r *http.Request) error {
	expected := os.Getenv("DATAFACT_API_KEY")
	if expected == "" {
		return errors.New("server misconfigured: missing DATAFACT_API_KEY")
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return errors.New("missing Authorization header")
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return errors.New("invalid Authorization format")
	}

	token := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	if token != expected {
		return errors.New("invalid API key")
	}

	return nil
}
// =====================
// 3. Env Helpers
// =====================

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustenv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("missing env: " + key)
	}
	return v
}

// =====================
// 4. Type Conversion Helpers
// =====================

func toStringSlice(v interface{}) ([]string, error) {
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, iv := range t {
			s, ok := iv.(string)
			if !ok {
				return nil, errors.New("array berisi non-string")
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, errors.New("harus string atau array string")
	}
}

func toBool(v interface{}) (bool, error) {
	if b, ok := v.(bool); ok {
		return b, nil
	}
	if s, ok := v.(string); ok {
		sl := strings.ToLower(strings.TrimSpace(s))
		if sl == "true" {
			return true, nil
		}
		if sl == "false" {
			return false, nil
		}
	}
	return false, errors.New("boolean tidak valid")
}

func toInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int, int32, int64:
		return strconv.ParseInt(fmt.Sprint(n), 10, 64)
	case string:
		return strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	default:
		return 0, errors.New("numerik tidak valid")
	}
}