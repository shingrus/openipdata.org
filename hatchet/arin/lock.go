package arin

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// skipBusy is the fixed reason of a task that found the directory in use.
const skipBusy = "busy"

// lock takes the lock of DataDir/arin for one task. The files there have no transaction: the worker
// and a run-arin started by hand must never write them together (two walks would also double the
// request rate). ok is false when another task or process holds the lock; the caller then skips.
func (p *Pipeline) lock() (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(p.dir(), lockFile), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
