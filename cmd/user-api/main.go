package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("HTTP_ADDR"); if addr == "" { addr = ":8080" }
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")) })
	s := &http.Server{Addr:addr, Handler:mux, ReadHeaderTimeout:5*time.Second}
	log.Fatal(s.ListenAndServe())
}
