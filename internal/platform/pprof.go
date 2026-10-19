package platform

import (
	"log"
	"net/http"
	"net/http/pprof"
)

// StartPprof поднимает pprof на :6060 (внутри контейнера, наружу не мапится).
func StartPprof() {
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("🔍 pprof на :6060")
		if err := http.ListenAndServe(":6060", mux); err != nil {
			log.Printf("⚠️ pprof: %v", err)
		}
	}()
}
