package main

import (
	"log"
	"net/http"
	"os"

	// Import package handler yang ada di folder api/v1
	// "datafact" adalah nama module yang kita init tadi
	handler "datafact/api/v1" 
)

func main() {
	// Definisikan Routing
	// Kita memanggil fungsi-fungsi dari package handler
	http.HandleFunc("/api/v1/persona-filter", handler.Handler)       // Ini fungsi Handler di persona-filter.go
	http.HandleFunc("/api/v1/form-scrapper", handler.ScrapperHandler) // Ini fungsi di form-scrapper.go
	http.HandleFunc("/api/v1/form-injector", handler.InjectorHandler) // Ini fungsi di form-injector.go

	// Tentukan Port (Google Cloud Run mewajibkan ambil dari environment variable PORT)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}