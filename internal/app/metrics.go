package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// withMetrics reserves a path under the existing system namespace. Scrapes
// always target this process; they must not follow repository shard routing.
func withMetrics(next, metrics http.Handler, token string) http.Handler {
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/system/metrics" {
			next.ServeHTTP(response, request)
			return
		}
		response.Header().Set("Cache-Control", "no-store")
		if token == "" {
			http.NotFound(response, request)
			return
		}
		values := request.Header.Values("Authorization")
		if len(values) != 1 || !validMetricsAuthorization(values[0], expected) {
			response.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		metrics.ServeHTTP(response, request)
	})
}

func validMetricsAuthorization(value string, expected [sha256.Size]byte) bool {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
}
