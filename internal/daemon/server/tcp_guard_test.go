package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBrowserCrossSiteGuard asserts the TCP-listener middleware rejects browser
// cross-site requests (the drive-by-web-page → RCE vector) while letting native
// clients (no Origin, no Sec-Fetch-Site) through.
func TestBrowserCrossSiteGuard(t *testing.T) {
	const okBody = "reached-handler"
	guarded := browserCrossSiteGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))

	cases := []struct {
		name       string
		host       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "native client no headers passes",
			host:       "localhost:8099",
			headers:    nil,
			wantStatus: http.StatusOK,
		},
		{
			name:       "sec-fetch-site cross-site rejected",
			host:       "localhost:8099",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "sec-fetch-site same-site rejected",
			host:       "localhost:8099",
			headers:    map[string]string{"Sec-Fetch-Site": "same-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "sec-fetch-site same-origin allowed",
			host:       "localhost:8099",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "sec-fetch-site none allowed",
			host:       "localhost:8099",
			headers:    map[string]string{"Sec-Fetch-Site": "none"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-origin Origin without sec-fetch rejected",
			host:       "localhost:8099",
			headers:    map[string]string{"Origin": "http://evil.example"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "same-origin Origin allowed",
			host:       "localhost:8099",
			headers:    map[string]string{"Origin": "http://localhost:8099"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "null Origin rejected",
			host:       "localhost:8099",
			headers:    map[string]string{"Origin": "null"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "different localhost port Origin rejected",
			host:       "localhost:8099",
			headers:    map[string]string{"Origin": "http://localhost:3000"},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// POST models the CORS-simple spawn request the attacker uses.
			req := httptest.NewRequest(http.MethodPost, "/api/agents/spawn", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			guarded.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusOK && rr.Body.String() != okBody {
				t.Fatalf("allowed request did not reach handler: body = %q", rr.Body.String())
			}
		})
	}
}
