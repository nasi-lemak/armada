package agent

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestClientCompressesOnceTheServerSaysSo(t *testing.T) {
	var mu sync.Mutex
	var encodings []string
	accept, refuse := true, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		enc := r.Header.Get("Content-Encoding")
		encodings = append(encodings, enc)
		var body io.Reader = r.Body
		if enc == "gzip" {
			if refuse {
				http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
				return
			}
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "bad gzip", 400)
				return
			}
			body = zr
		}
		var v map[string]any
		if err := json.NewDecoder(body).Decode(&v); err != nil || !strings.HasPrefix(v["pad"].(string), "xxxx") {
			http.Error(w, "bad body", 400)
			return
		}
		if accept {
			w.Header().Set("X-Armada-Accept-Encoding", "gzip")
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c, err := newClient(&Config{ServerURL: srv.URL}, func() string { return "k" })
	if err != nil {
		t.Fatal(err)
	}
	big := map[string]string{"pad": strings.Repeat("x", 2000)}
	small := map[string]string{"pad": "xxxx"}
	send := func(v any) {
		t.Helper()
		if err := c.do(t.Context(), "POST", "/x", v, nil); err != nil {
			t.Fatal(err)
		}
	}
	send(big)   // plain: the server hasn't said yet
	send(big)   // compressed
	send(small) // too small to bother
	mu.Lock()
	if got := strings.Join(encodings, ","); got != ",gzip," {
		t.Errorf("encodings %q, want plain, gzip, plain", got)
	}
	// A server that no longer accepts it (say, an older version restored).
	encodings, refuse, accept = nil, true, false
	mu.Unlock()
	send(big) // gzip refused, then plain
	send(big) // plain from now on
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(encodings, ","); got != "gzip,," {
		t.Errorf("after the server stopped accepting gzip: %q, want gzip, plain, plain", got)
	}
}
