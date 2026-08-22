// hop — short links (go.example/abc) and pastes (paste.example/xyz) in one
// small Go binary: SQLite, a bearer-token write API, a CLI, no JavaScript.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1:]))
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := openStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open %s: %v", cfg.DBPath, err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go st.janitor(ctx, 10*time.Minute)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newServer(cfg, st),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("hop %s listening on %s (links=%s pastes=%s db=%s writes=%v)",
		version, cfg.Listen, cfg.LinksHost, cfg.PasteHost, cfg.DBPath, cfg.Token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
