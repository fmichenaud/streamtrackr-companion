// fakeapi stands in for api.streamtrackr.com while testing the companion
// locally: it prints every push with the seconds since start — the timing
// is the point, since most of what the watch loop gets wrong shows up as
// an ordering or a delay — and can answer the failures that drive the
// retry, cancel and give-up paths.
//
// See tools/README.md for the scenarios.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var start = time.Now()

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "listen address (pass to the companion as -backend http://…)")
	mode := flag.String("mode", "ok", "ok | fail-unlock (500) | busy-relock (200 busy, retried) | no-route (404, a pre-2.9.0 server)")
	flag.Parse()

	http.HandleFunc("/api/companion/steam/unlock", func(w http.ResponseWriter, r *http.Request) {
		log("UNLOCK", r)
		if *mode == "fail-unlock" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply(w, 200, `{"status":"injected","trackerSlug":"fake"}`)
	})

	http.HandleFunc("/api/companion/steam/relock", func(w http.ResponseWriter, r *http.Request) {
		log("RELOCK", r)
		switch *mode {
		case "no-route":
			reply(w, 404, `{"statusCode":404,"message":"Cannot POST /api/companion/steam/relock"}`)
		case "busy-relock":
			reply(w, 200, `{"status":"busy","trackerSlug":"fake"}`)
		default:
			reply(w, 200, `{"status":"relocked","trackerSlug":"fake","relocked":1}`)
		}
	})

	// current-game keeps auto-detect mode usable; anything else is worth
	// seeing, since a companion talking to an endpoint we forgot about is
	// exactly what this harness is for.
	http.HandleFunc("/api/companion/current-game", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"appId":0}`)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("%7.3fs  OTHER   %s %s\n", time.Since(start).Seconds(), r.Method, r.URL.Path)
		w.WriteHeader(404)
	})

	fmt.Printf("fakeapi on http://%s (mode=%s)\n", *addr, *mode)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func log(kind string, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	fmt.Printf("%7.3fs  %s  %s\n", time.Since(start).Seconds(), kind, body)
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
