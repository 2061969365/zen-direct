package main

import (
	"fmt"
	"os"
	"sync"
)

// logrotate.go: 网关自管日志文件，硬顶预算（默认 200MB 总量）。
// stderr 重定向靠不住（Hidden 启动、外部轮转要停进程）；网关自己写文件、
// 按片轮转：单片满 -> .1/.2... 后移（最多 maxFiles 片），总量恒 <= perFile*maxFiles。
// LOG_FILE 设路径即启用（未设则沿用 stderr）；LOG_MAX_MB 总预算（默认 200，切 4 片）。
type rotateWriter struct {
	mu      sync.Mutex
	path    string
	perFile int64
	maxN    int
	f       *os.File
	size    int64
}

func newRotateWriterSized(path string, perFile int64, maxN int) (*rotateWriter, error) {
	if perFile < 1024 {
		perFile = 1024
	}
	if maxN < 2 {
		maxN = 2
	}
	w := &rotateWriter{path: path, perFile: perFile, maxN: maxN}
	if fi, err := os.Stat(path); err == nil {
		w.size = fi.Size()
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

func newRotateWriter(path string, budgetMB int) (*rotateWriter, error) {
	if budgetMB < 1 {
		budgetMB = 200
	}
	return newRotateWriterSized(path, int64(budgetMB)<<20/4, 4)
}

func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.perFile {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotateWriter) rotateLocked() error {
	_ = w.f.Close()
	// 最老丢弃，其余后移：.(maxN-1) 删，... .1 -> .2，当前 -> .1。
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxN-1))
	for i := w.maxN - 2; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	return nil
}

func (w *rotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
