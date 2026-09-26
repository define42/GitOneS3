package config

import "fmt"

// validateMetricsToken accepts an optional RFC 6750 bearer token. It deliberately
// does not trim whitespace: copying an unusable secret must fail at startup.
func validateMetricsToken(token string) error {
	if token == "" {
		return nil
	}
	if len(token) < 32 || len(token) > 1024 {
		return fmt.Errorf("config: GITONE_METRICS_TOKEN must contain 32 to 1024 ASCII bearer-token bytes")
	}
	padding := false
	for i := range len(token) {
		char := token[i]
		if char == '=' {
			padding = true
			continue
		}
		allowed := char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' || char == '-' || char == '.' || char == '_' ||
			char == '~' || char == '+' || char == '/'
		if padding || !allowed {
			return fmt.Errorf("config: GITONE_METRICS_TOKEN must be an ASCII bearer token without whitespace")
		}
	}
	if token[0] == '=' {
		return fmt.Errorf("config: GITONE_METRICS_TOKEN must contain bearer-token characters before padding")
	}
	return nil
}
