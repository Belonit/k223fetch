package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const progressWidth = 28

type progressBar struct {
	mu      sync.Mutex
	label   string
	total   int64
	current int64
	last    time.Time
}

func newProgress(label string, total int64) *progressBar {
	p := &progressBar{label: label, total: total}
	p.render(true, false)
	return p
}

func (p *progressBar) add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.current += n
	p.renderLocked(false, false)
	p.mu.Unlock()
}

func (p *progressBar) set(n int64) {
	p.mu.Lock()
	p.current = n
	p.renderLocked(false, false)
	p.mu.Unlock()
}

func (p *progressBar) done(err error) {
	p.mu.Lock()
	if err == nil && p.total > 0 {
		p.current = p.total
	}
	p.renderLocked(true, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  failed: %v", err)
	}
	fmt.Fprintln(os.Stderr)
	p.mu.Unlock()
}

func (p *progressBar) render(force, final bool) {
	p.mu.Lock()
	p.renderLocked(force, final)
	p.mu.Unlock()
}

func (p *progressBar) renderLocked(force, final bool) {
	if !force && time.Since(p.last) < 100*time.Millisecond {
		return
	}
	p.last = time.Now()
	filled := 0
	if p.total > 0 {
		filled = int(p.current * progressWidth / p.total)
		if filled > progressWidth {
			filled = progressWidth
		}
	}
	status := fmt.Sprintf("%d/%d", p.current, p.total)
	if p.total == 0 {
		status = formatBytes(p.current)
	}
	if p.total > 1024 {
		status = fmt.Sprintf("%s/%s", formatBytes(p.current), formatBytes(p.total))
	}
	if final && p.total == 0 {
		filled = progressWidth
	}
	fmt.Fprintf(os.Stderr, "\r[%-*s] %-24s %s", progressWidth, repeatBar(filled), p.label, status)
}

func repeatBar(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '#'
	}
	return string(b)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit && exp < 3; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

type progressReader struct {
	r io.Reader
	p *progressBar
}

func (r progressReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.p.add(int64(n))
	return n, err
}

func readFileProgress(path, label string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file is too large: %d bytes (limit %d)", info.Size(), limit)
	}
	p := newProgress(label, info.Size())
	b, err := io.ReadAll(io.LimitReader(progressReader{r: f, p: p}, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("file exceeds %d bytes", limit)
	}
	p.done(err)
	return b, err
}
