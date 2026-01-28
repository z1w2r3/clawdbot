package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clawdbot/relay/relay"
)

func main() {
	port := os.Getenv("RELAY_PORT")
	if port == "" {
		port = "9000"
	}
	secret := os.Getenv("RELAY_SECRET")
	if secret == "" {
		log.Fatal("RELAY_SECRET is required")
	}

	hub := relay.NewHub(secret)

	mux := http.NewServeMux()
	mux.HandleFunc("/gateway/register", hub.HandleGatewayRegister)
	mux.HandleFunc("/gateway/tunnel", hub.HandleGatewayTunnel)
	mux.HandleFunc("/client/connect", hub.HandleClientConnect)
	mux.HandleFunc("/client/reconnect", hub.HandleClientReconnect)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux}

	tlsCert := os.Getenv("TLS_CERT")
	tlsKey := os.Getenv("TLS_KEY")

	go func() {
		if tlsCert != "" && tlsKey != "" {
			log.Printf("relay server listening on :%s (TLS)", port)
			if err := srv.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen TLS: %v", err)
			}
		} else {
			log.Printf("relay server listening on :%s", port)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen: %v", err)
			}
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down relay server")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	hub.Close()
}
