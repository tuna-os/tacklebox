package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// referrersServer serves the token endpoint, one referrers index and 404
// everywhere else.
func referrersServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(status int, ctype, body string) {
			w.Header().Set("Content-Type", ctype)
			w.WriteHeader(status)
			if _, err := io.WriteString(w, body); err != nil {
				panic(err)
			}
		}
		switch {
		case r.URL.Path == "/token":
			write(http.StatusOK, "application/json", `{"token":"fixture-token"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/repo/referrers/"):
			write(http.StatusOK, "application/vnd.oci.image.index.v1+json", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
				`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","digest":"sha256:abc","size":42}]}`)
		default:
			write(http.StatusNotFound, "text/plain", "fixture path not found")
		}
	}))
}

func TestReferrers(t *testing.T) {
	srv := referrersServer()
	defer srv.Close()
	c := NewClient(srv.URL)
	got, err := c.Referrers(Ref{Repo: "repo", Tag: "t"}, "sha256:def")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Digest != "sha256:abc" {
		t.Fatalf("unexpected referrers: %+v", got)
	}
}

func TestReferrersNotFound(t *testing.T) {
	srv := referrersServer()
	defer srv.Close()
	c := NewClient(srv.URL)
	// Unknown digests 404: no referrers, not an error.
	got, err := c.Referrers(Ref{Repo: "other", Tag: "t"}, "sha256:def")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected referrers: %+v", got)
	}
}
