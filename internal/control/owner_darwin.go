package control

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrOwnerRunning = errors.New("the desktop app is already running")

type Owner struct {
	dir  string
	lock *os.File
}

func Acquire(dir string) (*Owner, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := checkStateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "owner.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if err := checkPrivateFile(file); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrOwnerRunning
		}
		return nil, err
	}
	return &Owner{dir: dir, lock: file}, nil
}

func (o *Owner) Close() error {
	return o.lock.Close()
}

func ownedByUser(info os.FileInfo) bool {
	return info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid())
}

func processStartTime(pid int) (int64, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return info.Proc.P_starttime.Nano(), nil
}
