//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !illumos

package cache

import (
	"fmt"
	"runtime"
)

// New calls acquireLock only when the disk tier is enabled. Other platforms
// retain memory caching without relying on an unavailable filesystem lock.
func (*Cache) acquireLock() error {
	return fmt.Errorf("cache: disk caching is unsupported on %s; set the disk budget to zero to use memory caching", runtime.GOOS)
}
