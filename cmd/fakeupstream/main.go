// Command fakeupstream serves recorded OpenAI/Anthropic responses for
// integration and load tests.
//
//	go run ./cmd/fakeupstream -listen :9999 -delay 5ms
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/garritfra/llmtee/internal/fakeupstream"
)

func main() {
	listen := flag.String("listen", ":9999", "listen address")
	delay := flag.Duration("delay", 0, "delay between stream chunks")
	flag.Parse()
	srv := &fakeupstream.Server{ChunkDelay: *delay}
	log.Printf("fake upstream on %s", *listen)
	hs := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.ListenAndServe())
}
