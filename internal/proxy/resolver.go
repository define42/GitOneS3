package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/define42/GitOneS3/internal/shard"
)

const defaultServiceDomain = "svc"

var ErrInvalidResolverConfig = errors.New("proxy: invalid statefulset resolver configuration")

// StatefulSetResolverOptions describes stable Kubernetes pod DNS names.
type StatefulSetResolverOptions struct {
	Scheme          string
	StatefulSet     string
	HeadlessService string
	Namespace       string
	ServiceDomain   string
	Port            uint16
}

// StatefulSetResolver resolves a shard to its stable StatefulSet pod address.
type StatefulSetResolver struct {
	scheme          string
	statefulSet     string
	headlessService string
	namespace       string
	serviceDomain   string
	port            uint16
}

// NewStatefulSetResolver validates and constructs a stable pod resolver.
func NewStatefulSetResolver(options StatefulSetResolverOptions) (*StatefulSetResolver, error) {
	if options.Scheme != "http" && options.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme must be http or https", ErrInvalidResolverConfig)
	}
	if options.Port == 0 {
		return nil, fmt.Errorf("%w: port must be positive", ErrInvalidResolverConfig)
	}
	if !isDNSLabel(options.StatefulSet) {
		return nil, fmt.Errorf("%w: invalid statefulset name", ErrInvalidResolverConfig)
	}
	if !isDNSLabel(options.HeadlessService) {
		return nil, fmt.Errorf("%w: invalid headless service name", ErrInvalidResolverConfig)
	}
	if !isDNSLabel(options.Namespace) {
		return nil, fmt.Errorf("%w: invalid namespace", ErrInvalidResolverConfig)
	}

	serviceDomain := options.ServiceDomain
	if serviceDomain == "" {
		serviceDomain = defaultServiceDomain
	}
	if !isDNSName(serviceDomain) {
		return nil, fmt.Errorf("%w: invalid service domain", ErrInvalidResolverConfig)
	}

	return &StatefulSetResolver{
		scheme:          options.Scheme,
		statefulSet:     options.StatefulSet,
		headlessService: options.HeadlessService,
		namespace:       options.Namespace,
		serviceDomain:   serviceDomain,
		port:            options.Port,
	}, nil
}

// Resolve returns a new destination URL for id.
func (r *StatefulSetResolver) Resolve(id shard.ShardID) (*url.URL, error) {
	podName := r.statefulSet + "-" + strconv.FormatUint(uint64(id), 10)
	if !isDNSLabel(podName) {
		return nil, fmt.Errorf("%w: generated pod name is invalid", ErrInvalidResolverConfig)
	}
	host := strings.Join(
		[]string{podName, r.headlessService, r.namespace, r.serviceDomain},
		".",
	)
	return &url.URL{
		Scheme: r.scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(int(r.port))),
	}, nil
}

func isDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !isDNSLabel(label) {
			return false
		}
	}
	return true
}

func isDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	for index := 0; index < len(label); index++ {
		value := label[index]
		isLetter := value >= 'a' && value <= 'z'
		isDigit := value >= '0' && value <= '9'
		isHyphen := value == '-'
		if !isLetter && !isDigit && !isHyphen {
			return false
		}
		if isHyphen && (index == 0 || index == len(label)-1) {
			return false
		}
	}
	return true
}
