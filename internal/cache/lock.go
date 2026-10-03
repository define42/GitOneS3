//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || illumos

package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func (c *Cache) acquireLock() error {
	file, err := os.OpenFile(filepath.Join(c.directory, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open cache ownership lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Join(fmt.Errorf("cache directory is already in use: %w", err), file.Close())
	}
	c.lock = file
	return nil
}
