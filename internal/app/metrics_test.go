package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/metrics"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestWithMetrics(t *testing.T) {
	t.Parallel()
	const token = "secret-metrics-token-for-tests-123456"
	for _, test := range []struct {
		name    string
		token   string
		path    string
		method  string
		headers []string
		want    int
		metrics bool
		next    bool
	}{
		{name: "disabled", path: "/system/metrics", headers: []string{"Bearer " + token}, want: http.StatusNotFound},
		{name: "no authorization", token: token, path: "/system/metrics", want: http.StatusUnauthorized},
		{name: "wrong token", token: token, path: "/system/metrics", headers: []string{"Bearer wrong"}, want: http.StatusUnauthorized},
		{name: "wrong scheme", token: token, path: "/system/metrics", headers: []string{"Basic " + token}, want: http.StatusUnauthorized},
		{name: "extra space", token: token, path: "/system/metrics", headers: []string{"Bearer  " + token}, want: http.StatusUnauthorized},
		{name: "duplicate authorization", token: token, path: "/system/metrics", headers: []string{"Bearer " + token, "Bearer " + token}, want: http.StatusUnauthorized},
		{name: "query token ignored", token: token, path: "/system/metrics?token=" + token, want: http.StatusUnauthorized},
		{name: "authorized", token: token, path: "/system/metrics", headers: []string{"Bearer " + token}, want: http.StatusOK, metrics: true},
		{name: "scheme case insensitive", token: token, path: "/system/metrics", headers: []string{"bEaReR " + token}, want: http.StatusOK, metrics: true},
		{name: "head", token: token, path: "/system/metrics", method: http.MethodHead, headers: []string{"Bearer " + token}, want: http.StatusOK},
		{name: "unsupported method", token: token, path: "/system/metrics", method: http.MethodPost, headers: []string{"Bearer " + token}, want: http.StatusMethodNotAllowed},
		{name: "normal namespace", token: token, path: "/metrics/repo.git", want: http.StatusAccepted, next: true},
		{name: "other system path", token: token, path: "/system/other", want: http.StatusAccepted, next: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, err := metrics.NewStore(storage.NewMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			calledNext := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calledNext = true
				w.WriteHeader(http.StatusAccepted)
			})
			method := test.method
			if method == "" {
				method = http.MethodGet
			}
			request := httptest.NewRequestWithContext(t.Context(), method, test.path, nil)
			for _, header := range test.headers {
				request.Header.Add("Authorization", header)
			}
			response := httptest.NewRecorder()
			withMetrics(next, store, test.token).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if calledNext != test.next {
				t.Fatalf("next called = %v, want %v", calledNext, test.next)
			}
			if got := strings.Contains(response.Body.String(), "gitone_storage_operations_total"); got != test.metrics {
				t.Fatalf("metrics returned = %v, want %v", got, test.metrics)
			}
			if response.Code == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != `Bearer realm="metrics"` {
				t.Fatal("missing bearer authentication challenge")
			}
			if strings.Contains(response.Body.String(), token) {
				t.Fatal("response discloses token")
			}
		})
	}
}
