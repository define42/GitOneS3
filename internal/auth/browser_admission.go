package auth

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Browsing validates manifests up to 64 MiB. Bound all manifest-loading API
// routes together, independently of the Git transfer and LFS admission gates.
const maxBrowserReads = 4

func (s *Service) acquireBrowser(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, repositoryAPIError(err)
	}
	select {
	case s.browserReads <- struct{}{}:
		release := func() { <-s.browserReads }
		if err := ctx.Err(); err != nil {
			release()
			return nil, repositoryAPIError(err)
		}
		return release, nil
	default:
		return nil, huma.ErrorWithHeaders(
			huma.Error503ServiceUnavailable("repository browser is busy; retry shortly"),
			http.Header{"Retry-After": {"1"}},
		)
	}
}
