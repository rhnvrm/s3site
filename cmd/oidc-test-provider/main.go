// oidc-test-provider is a local-only, passwordless test fixture, not an identity service.
package main

import (
	"github.com/rhnvrm/s3site/internal/oidctest"
	"log"
	"net/http"
	"time"
)

func main() {
	p, err := oidctest.New("http://127.0.0.1:5556")
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Insecure test provider listening on http://127.0.0.1:5556 (local development only)")
	server := &http.Server{Addr: "127.0.0.1:5556", Handler: p, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
