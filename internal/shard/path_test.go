package shard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestParserParseValidPaths(t *testing.T) {
	t.Parallel()

	parser := newTestParser(t)
	tests := []struct {
		name               string
		target             string
		expectedTopLevel   string
		expectedRepository string
	}{
		{
			name:               "root repository discovery",
			target:             "/alice/project.git/info/refs?service=git-upload-pack",
			expectedTopLevel:   "alice",
			expectedRepository: "alice/project",
		},
		{
			name:               "nested repository receive pack",
			target:             "/acme/platform/linux.git/git-receive-pack",
			expectedTopLevel:   "acme",
			expectedRepository: "acme/platform/linux",
		},
		{
			name:               "lfs endpoint",
			target:             "/team-42/apps/web.git/info/lfs/objects/batch",
			expectedTopLevel:   "team-42",
			expectedRepository: "team-42/apps/web",
		},
		{
			name:             "namespace request without git suffix",
			target:           "/alice/projects",
			expectedTopLevel: "alice",
		},
		{
			name:             "maximum length top level",
			target:           "/" + strings.Repeat("a", DefaultMaxTopLevelLength),
			expectedTopLevel: strings.Repeat("a", DefaultMaxTopLevelLength),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := newRequest(t, test.target)
			actual, err := parser.Parse(request)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if actual.TopLevel != test.expectedTopLevel {
				t.Errorf("TopLevel = %q, expected %q", actual.TopLevel, test.expectedTopLevel)
			}
			if actual.Repository != test.expectedRepository {
				t.Errorf("Repository = %q, expected %q", actual.Repository, test.expectedRepository)
			}
		})
	}
}

func TestParserParseRejectsAmbiguousPaths(t *testing.T) {
	t.Parallel()

	parser := newTestParser(t)
	overlongName := strings.Repeat("a", DefaultMaxTopLevelLength+1)
	overlongComponent := strings.Repeat("a", DefaultMaxComponentLength+1)
	overdeepPath := "/alice/" + strings.Repeat("group/", DefaultMaxPathDepth-1) + "repo.git"
	tests := []struct {
		name    string
		request func(*testing.T) *http.Request
	}{
		{name: "nil request", request: func(*testing.T) *http.Request { return nil }},
		{
			name: "nil url",
			request: func(*testing.T) *http.Request {
				return &http.Request{}
			},
		},
		{name: "root only", request: targetRequest("/")},
		{name: "empty top level", request: targetRequest("//repo.git")},
		{name: "empty descendant", request: targetRequest("/alice//repo.git")},
		{name: "trailing slash", request: targetRequest("/alice/repo.git/")},
		{name: "dot component", request: targetRequest("/alice/./repo.git")},
		{name: "dot dot component", request: targetRequest("/alice/../repo.git")},
		{name: "uppercase top level", request: targetRequest("/Alice/repo.git")},
		{name: "non ascii top level", request: targetRequest("/al%C3%AFce/repo.git")},
		{name: "underscore top level", request: targetRequest("/alice_team/repo.git")},
		{name: "leading hyphen", request: targetRequest("/-alice/repo.git")},
		{name: "trailing hyphen", request: targetRequest("/alice-/repo.git")},
		{name: "overlong top level", request: targetRequest("/" + overlongName + "/repo.git")},
		{name: "overlong descendant", request: targetRequest("/alice/" + overlongComponent + ".git")},
		{name: "path exceeds depth limit", request: targetRequest(overdeepPath)},
		{name: "reserved top level", request: targetRequest("/api/repo.git")},
		{name: "git suffix on top level", request: targetRequest("/alice.git/repo")},
		{name: "uppercase git suffix", request: targetRequest("/alice/repo.GIT/info/refs")},
		{name: "empty repository name", request: targetRequest("/alice/.git/info/refs")},
		{name: "repeated git suffix", request: targetRequest("/alice/repo.git.git/info/refs")},
		{name: "multiple git suffixes", request: targetRequest("/alice/repo.git/other.git")},
		{name: "encoded top level byte", request: targetRequest("/%61lice/repo.git")},
		{name: "encoded slash", request: targetRequest("/alice%2Frepo/project.git")},
		{name: "encoded backslash", request: targetRequest("/alice%5Crepo/project.git")},
		{name: "encoded traversal", request: targetRequest("/alice/%2e%2e/repo.git")},
		{name: "encoded git dot", request: targetRequest("/alice/repo%2egit/info/refs")},
		{
			name: "invalid percent escape",
			request: func(*testing.T) *http.Request {
				return &http.Request{
					RequestURI: "/alice/%zz",
					URL:        &url.URL{Path: "/alice/%zz"},
				}
			},
		},
		{
			name: "raw path mismatch",
			request: func(*testing.T) *http.Request {
				return &http.Request{
					URL: &url.URL{
						Path:    "/alice/repo.git",
						RawPath: "/bob/repo.git",
					},
				}
			},
		},
		{
			name: "raw backslash",
			request: func(*testing.T) *http.Request {
				return &http.Request{
					RequestURI: "/alice\\repo/project.git",
					URL:        &url.URL{Path: "/alice\\repo/project.git"},
				}
			},
		},
		{
			name: "raw control byte",
			request: func(*testing.T) *http.Request {
				return &http.Request{
					RequestURI: "/alice/repo\x00.git",
					URL:        &url.URL{Path: "/alice/repo\x00.git"},
				}
			},
		},
		{
			name: "opaque url",
			request: func(*testing.T) *http.Request {
				return &http.Request{URL: &url.URL{Opaque: "/alice/repo.git"}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parser.Parse(test.request(t))
			if !errors.Is(err, ErrInvalidPath) && !errors.Is(err, ErrReservedTopLevel) {
				t.Fatalf("Parse() error = %v, expected invalid path or reserved name", err)
			}
		})
	}
}

func TestParserDepthCountsRepositoryPathNotProtocolSuffix(t *testing.T) {
	t.Parallel()

	policy := DefaultPathPolicy()
	policy.MaxPathDepth = 2
	parser, err := NewParser(policy)
	if err != nil {
		t.Fatalf("NewParser() error = %v", err)
	}

	for _, target := range []string{
		"/alice/repo.git/info/refs",
		"/alice/repo.git/info/lfs/objects/batch",
	} {
		if _, err := parser.Parse(newRequest(t, target)); err != nil {
			t.Errorf("Parse(%q) error = %v, want protocol suffix excluded from depth", target, err)
		}
	}
	for _, target := range []string{
		"/alice/group/repo.git/info/refs",
		"/alice/projects/more",
	} {
		if _, err := parser.Parse(newRequest(t, target)); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Parse(%q) error = %v, want namespace depth error", target, err)
		}
	}

	overlongTail := "/alice/repo.git/" + strings.Repeat("tail/", maxProtocolTailDepth) + "tail"
	if _, err := parser.Parse(newRequest(t, overlongTail)); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Parse(overlong protocol tail) error = %v, want depth error", err)
	}
}

func TestNewParserValidatesPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PathPolicy)
	}{
		{
			name:   "zero maximum length",
			mutate: func(policy *PathPolicy) { policy.MaxTopLevelLength = 0 },
		},
		{
			name:   "zero component length",
			mutate: func(policy *PathPolicy) { policy.MaxComponentLength = 0 },
		},
		{
			name:   "depth below repository minimum",
			mutate: func(policy *PathPolicy) { policy.MaxPathDepth = 1 },
		},
		{
			name: "top-level exceeds component length",
			mutate: func(policy *PathPolicy) {
				policy.MaxTopLevelLength = policy.MaxComponentLength + 1
			},
		},
		{
			name: "invalid reserved name",
			mutate: func(policy *PathPolicy) {
				policy.ReservedNames = []string{"Not-Canonical"}
			},
		},
		{
			name: "reserved name longer than maximum",
			mutate: func(policy *PathPolicy) {
				policy.MaxTopLevelLength = 2
				policy.ReservedNames = []string{"api"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policy := DefaultPathPolicy()
			test.mutate(&policy)
			_, err := NewParser(policy)
			if !errors.Is(err, ErrInvalidPathPolicy) {
				t.Fatalf("NewParser() error = %v, expected ErrInvalidPathPolicy", err)
			}
		})
	}
}

func newTestParser(t *testing.T) *Parser {
	t.Helper()
	parser, err := NewParser(DefaultPathPolicy())
	if err != nil {
		t.Fatalf("NewParser() error = %v", err)
	}
	return parser
}

func newRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://gitone.test"+target, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	return request
}

func targetRequest(target string) func(*testing.T) *http.Request {
	return func(t *testing.T) *http.Request {
		return newRequest(t, target)
	}
}
