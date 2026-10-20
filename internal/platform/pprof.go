package platform

import (
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"
)

// StartPprof поднимает pprof только при PPROF_ENABLED=true и по умолчанию
// только на loopback: heap/goroutine-дампы содержат JWT_SECRET, AUTH_PASSWORD
// и хэши PIN. Для доступа снаружи контейнера задайте PPROF_ADDR осознанно.
func StartPprof() {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("PPROF_ENABLED")), "true") {
		return
	}

	addr := GetEnv("PPROF_ADDR", "127.0.0.1:6060")

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Printf("🔍 pprof на %s", addr)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("⚠️ pprof: %v", err)
		}
	}()
}
