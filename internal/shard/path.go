package shard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxTopLevelLength  = 63
	DefaultMaxComponentLength = 255
	DefaultMaxPathDepth       = 32
	maxProtocolTailDepth      = 16
)

var (
	ErrInvalidPath       = errors.New("shard: invalid request path")
	ErrInvalidPathPolicy = errors.New("shard: invalid path policy")
	ErrReservedTopLevel  = errors.New("shard: reserved top-level name")
)

// PathPolicy controls permanent namespace syntax and repository nesting.
type PathPolicy struct {
	MaxTopLevelLength  int
	MaxComponentLength int
	MaxPathDepth       int
	ReservedNames      []string
}

// DefaultPathPolicy returns the conservative policy recommended by the
// architecture. Callers may replace it with installation-specific policy.
func DefaultPathPolicy() PathPolicy {
	return PathPolicy{
		MaxTopLevelLength:  DefaultMaxTopLevelLength,
		MaxComponentLength: DefaultMaxComponentLength,
		MaxPathDepth:       DefaultMaxPathDepth,
		ReservedNames:      []string{"api", "gitone", "system"},
	}
}

// RequestPath is the canonical routing information extracted from a request.
// Repository is slash-separated and has its terminal .git suffix removed. It
// is empty for a request that does not contain a repository component.
type RequestPath struct {
	TopLevel   string
	Repository string
}

// Parser validates request paths using one immutable namespace policy.
type Parser struct {
	maxTopLevelLength  int
	maxComponentLength int
	maxPathDepth       int
	reservedNames      map[string]struct{}
}

// NewParser constructs a request-path parser and validates its policy.
func NewParser(policy PathPolicy) (*Parser, error) {
	if policy.MaxTopLevelLength <= 0 {
		return nil, fmt.Errorf(
			"%w: maximum top-level length must be positive",
			ErrInvalidPathPolicy,
		)
	}
	if policy.MaxComponentLength <= 0 {
		return nil, fmt.Errorf(
			"%w: maximum component length must be positive",
			ErrInvalidPathPolicy,
		)
	}
	if policy.MaxPathDepth < 2 {
		return nil, fmt.Errorf(
			"%w: maximum path depth must be at least two",
			ErrInvalidPathPolicy,
		)
	}
	if policy.MaxTopLevelLength > policy.MaxComponentLength {
		return nil, fmt.Errorf(
			"%w: top-level length cannot exceed component length",
			ErrInvalidPathPolicy,
		)
	}

	reservedNames := make(map[string]struct{}, len(policy.ReservedNames))
	for _, name := range policy.ReservedNames {
		if err := validateTopLevel(name, policy.MaxTopLevelLength); err != nil {
			return nil, fmt.Errorf(
				"%w: reserved name %q: %v",
				ErrInvalidPathPolicy,
				name,
				err,
			)
		}
		reservedNames[name] = struct{}{}
	}

	return &Parser{
		maxTopLevelLength:  policy.MaxTopLevelLength,
		maxComponentLength: policy.MaxComponentLength,
		maxPathDepth:       policy.MaxPathDepth,
		reservedNames:      reservedNames,
	}, nil
}

// Parse extracts and validates canonical routing information from request.
func (p *Parser) Parse(request *http.Request) (RequestPath, error) {
	if request == nil || request.URL == nil {
		return RequestPath{}, fmt.Errorf("%w: missing request url", ErrInvalidPath)
	}
	if request.URL.Opaque != "" {
		return RequestPath{}, fmt.Errorf("%w: opaque url is not supported", ErrInvalidPath)
	}

	escapedPaths, err := requestEscapedPaths(request)
	if err != nil {
		return RequestPath{}, err
	}
	for _, escapedPath := range escapedPaths {
		if err := validateEscapedPath(escapedPath, request.URL.Path); err != nil {
			return RequestPath{}, err
		}
	}

	components, err := splitDecodedPath(request.URL.Path, p.maxComponentLength)
	if err != nil {
		return RequestPath{}, err
	}
	if err := validateTopLevel(components[0], p.maxTopLevelLength); err != nil {
		return RequestPath{}, err
	}
	if _, isReserved := p.reservedNames[components[0]]; isReserved {
		return RequestPath{}, fmt.Errorf(
			"%w: %q",
			ErrReservedTopLevel,
			components[0],
		)
	}

	repository, repositoryDepth, err := canonicalRepository(components)
	if err != nil {
		return RequestPath{}, err
	}
	namespaceDepth := len(components)
	protocolTailDepth := 0
	if repositoryDepth > 0 {
		namespaceDepth = repositoryDepth
		protocolTailDepth = len(components) - repositoryDepth
	}
	if namespaceDepth > p.maxPathDepth {
		return RequestPath{}, fmt.Errorf(
			"%w: namespace exceeds maximum depth",
			ErrInvalidPath,
		)
	}
	if protocolTailDepth > maxProtocolTailDepth {
		return RequestPath{}, fmt.Errorf(
			"%w: protocol suffix exceeds maximum depth",
			ErrInvalidPath,
		)
	}

	return RequestPath{
		TopLevel:   components[0],
		Repository: repository,
	}, nil
}

func requestEscapedPaths(request *http.Request) ([]string, error) {
	escapedPaths := make([]string, 0, 2)
	if request.RequestURI != "" {
		path, _, _ := strings.Cut(request.RequestURI, "?")
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "#") {
			return nil, fmt.Errorf("%w: invalid request target", ErrInvalidPath)
		}
		escapedPaths = append(escapedPaths, path)
	}
	if request.URL.RawPath != "" {
		escapedPaths = append(escapedPaths, request.URL.RawPath)
	}
	if len(escapedPaths) == 0 {
		escapedPaths = append(escapedPaths, request.URL.EscapedPath())
	}
	return escapedPaths, nil
}

func validateEscapedPath(escapedPath, decodedPath string) error {
	if !strings.HasPrefix(escapedPath, "/") {
		return fmt.Errorf("%w: path must be absolute", ErrInvalidPath)
	}

	for index := 0; index < len(escapedPath); index++ {
		if escapedPath[index] != '%' {
			continue
		}
		if index+2 >= len(escapedPath) {
			return fmt.Errorf("%w: incomplete percent escape", ErrInvalidPath)
		}

		value, ok := decodeHexByte(escapedPath[index+1], escapedPath[index+2])
		if !ok {
			return fmt.Errorf("%w: invalid percent escape", ErrInvalidPath)
		}
		switch value {
		case '/', '\\', '.', 0, 0x7f:
			return fmt.Errorf("%w: ambiguous escaped path byte", ErrInvalidPath)
		}
		if value < 0x20 {
			return fmt.Errorf("%w: escaped control byte", ErrInvalidPath)
		}
		index += 2
	}

	unescapedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return fmt.Errorf("%w: invalid percent encoding", ErrInvalidPath)
	}
	if unescapedPath != decodedPath {
		return fmt.Errorf("%w: escaped and decoded paths differ", ErrInvalidPath)
	}

	rawComponents := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(rawComponents) == 0 || strings.Contains(rawComponents[0], "%") {
		return fmt.Errorf("%w: top-level name must be unescaped", ErrInvalidPath)
	}

	return nil
}

func splitDecodedPath(path string, maxComponentLength int) ([]string, error) {
	if !utf8.ValidString(path) || !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w: invalid decoded path", ErrInvalidPath)
	}

	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(components) == 0 {
		return nil, fmt.Errorf("%w: missing top-level name", ErrInvalidPath)
	}
	for _, component := range components {
		isAmbiguous := component == "" || component == "." || component == ".."
		if isAmbiguous || strings.ContainsAny(component, "\\%") {
			return nil, fmt.Errorf("%w: ambiguous path component", ErrInvalidPath)
		}
		if strings.IndexFunc(component, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: control byte in path component", ErrInvalidPath)
		}
		if len(component) > maxComponentLength {
			return nil, fmt.Errorf("%w: path component is too long", ErrInvalidPath)
		}
	}

	return components, nil
}

func validateTopLevel(name string, maxLength int) error {
	if name == "" || len(name) > maxLength {
		return fmt.Errorf("%w: invalid top-level length", ErrInvalidPath)
	}
	for index := 0; index < len(name); index++ {
		value := name[index]
		isLetter := value >= 'a' && value <= 'z'
		isDigit := value >= '0' && value <= '9'
		isHyphen := value == '-'
		if !isLetter && !isDigit && !isHyphen {
			return fmt.Errorf("%w: top-level name is not lowercase ascii", ErrInvalidPath)
		}
		if isHyphen && (index == 0 || index == len(name)-1) {
			return fmt.Errorf("%w: top-level name has an edge hyphen", ErrInvalidPath)
		}
	}
	return nil
}

func canonicalRepository(components []string) (string, int, error) {
	var repository string
	repositoryDepth := 0
	for index, component := range components {
		if len(component) < len(".git") {
			continue
		}
		suffix := component[len(component)-len(".git"):]
		if !strings.EqualFold(suffix, ".git") {
			continue
		}
		if suffix != ".git" || index == 0 || repository != "" {
			return "", 0, fmt.Errorf("%w: non-canonical repository suffix", ErrInvalidPath)
		}

		name := strings.TrimSuffix(component, ".git")
		if name == "" || name == "." || name == ".." || strings.HasSuffix(name, ".git") {
			return "", 0, fmt.Errorf("%w: invalid repository name", ErrInvalidPath)
		}

		canonicalComponents := append([]string{}, components[:index]...)
		canonicalComponents = append(canonicalComponents, name)
		repository = strings.Join(canonicalComponents, "/")
		repositoryDepth = index + 1
	}
	return repository, repositoryDepth, nil
}

func decodeHexByte(high, low byte) (byte, bool) {
	highValue, highOK := decodeHexDigit(high)
	lowValue, lowOK := decodeHexDigit(low)
	if !highOK || !lowOK {
		return 0, false
	}
	return highValue<<4 | lowValue, true
}

func decodeHexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
