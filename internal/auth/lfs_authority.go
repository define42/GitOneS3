package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/shard"
)

func (s *Service) verifyRemoteLFSGrant(ctx context.Context, owner shard.ShardID, grant lfsGrant) error {
	unavailable := huma.Error503ServiceUnavailable("SSH key authority unavailable")
	if s.tokenResolver == nil {
		return unavailable
	}
	// The owning shard already checked admission expiry. Sign a separate,
	// short-lived authority request so finalization can recheck a key after a
	// long transfer without making expired client credentials reusable.
	grant.Expires = time.Now().Add(lfsKeyCheckLifetime).Unix()
	raw, err := s.sessionCodec.Encode(lfsKeyCheckLabel, grant)
	if err != nil {
		return unavailable
	}
	destination, err := s.tokenResolver.Resolve(owner)
	if err != nil || destination == nil || (destination.Scheme != "http" && destination.Scheme != "https") ||
		destination.Host == "" || destination.User != nil || destination.Opaque != "" {
		return unavailable
	}
	u := *destination
	u.Path, u.RawPath = "/api/v1/users/"+grant.Principal.Username+"/lfs/verify", ""
	u.RawQuery, u.Fragment = "", ""
	// #nosec G704 -- Only the deployment's shard resolver controls the destination.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return unavailable
	}
	request.Header.Set("Authorization", "Bearer "+lfsKeyCheckPrefix+raw)
	// #nosec G704 -- The resolver supplies the authority URL and the client refuses redirects.
	response, err := s.tokenClient.Do(request)
	if err != nil {
		return unavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return huma.Error401Unauthorized("invalid LFS credential")
	}
	if response.StatusCode != http.StatusNoContent {
		return unavailable
	}
	return nil
}
