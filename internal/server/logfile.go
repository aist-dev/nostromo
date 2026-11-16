package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingFile is an append-only log file that rotates by size, keeping Keep old files
// (path.1 is the newest of them).
type RotatingFile struct {
	Path     string
	MaxBytes int64
	Keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotating opens (or creates) a rotating log file.
func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{Path: path, MaxBytes: maxBytes, Keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *RotatingFile) rotate() error {
	r.f.Close()
	for i := r.Keep; i >= 1; i-- {
		src := r.Path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", r.Path, i-1)
		}
		os.Rename(src, fmt.Sprintf("%s.%d", r.Path, i))
	}
	if r.Keep == 0 {
		os.Remove(r.Path)
	}
	return r.open()
}

// Write appends p, rotating first if the file would exceed MaxBytes.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.MaxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
