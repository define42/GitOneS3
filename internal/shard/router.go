package shard

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrInvalidShardCount = errors.New("shard: shard count must be positive")
	ErrMissingParser     = errors.New("shard: parser is required")
)

// ShardID identifies a configured logical shard.
type ShardID uint32

// Route is the deterministic owner and canonical path for one request.
type Route struct {
	Owner ShardID
	Path  RequestPath
}

// Router maps canonical top-level namespace names to logical shards.
type Router struct {
	shardCount uint32
	parser     *Parser
}

// NewRouter constructs a deterministic router.
func NewRouter(shardCount uint32, parser *Parser) (*Router, error) {
	if shardCount == 0 {
		return nil, ErrInvalidShardCount
	}
	if parser == nil {
		return nil, ErrMissingParser
	}
	return &Router{
		shardCount: shardCount,
		parser:     parser,
	}, nil
}

// ShardCount returns the immutable configured shard count.
func (r *Router) ShardCount() uint32 {
	return r.shardCount
}

// Owner returns the owner for a canonical top-level name.
func (r *Router) Owner(topLevel string) (ShardID, error) {
	if err := validateTopLevel(topLevel, r.parser.maxTopLevelLength); err != nil {
		return 0, err
	}
	if _, isReserved := r.parser.reservedNames[topLevel]; isReserved {
		return 0, fmt.Errorf("%w: %q", ErrReservedTopLevel, topLevel)
	}
	// #nosec G115 -- The remainder is below the nonzero uint32 shard count, so it fits ShardID.
	return ShardID(Sum64([]byte(topLevel)) % uint64(r.shardCount)), nil
}

// Resolve parses request and returns its deterministic owner.
func (r *Router) Resolve(request *http.Request) (Route, error) {
	path, err := r.parser.Parse(request)
	if err != nil {
		return Route{}, err
	}
	owner, err := r.Owner(path.TopLevel)
	if err != nil {
		return Route{}, err
	}
	return Route{
		Owner: owner,
		Path:  path,
	}, nil
}
