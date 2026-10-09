package krunlet

import (
	"bytes"
	"io"
	"sync"
)

type outputBudget struct {
	mu          sync.Mutex
	limit, used int64
	over        bool
	kill        func()
}

func (b *outputBudget) setKill(k func()) {
	b.mu.Lock()
	b.kill = k
	over := b.over
	b.mu.Unlock()
	if over {
		k()
	}
}
func (b *outputBudget) exceeded() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.over }
func (b *outputBudget) allow(n int) (int, func()) {
	b.mu.Lock()
	available := b.limit - b.used
	take := int64(n)
	if take > available {
		take = available
	}
	if take < 0 {
		take = 0
	}
	b.used += take
	var kill func()
	if int64(n) > take && !b.over {
		b.over = true
		kill = b.kill
	}
	b.mu.Unlock()
	return int(take), kill
}

type limitedWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	budget *outputBudget
	mirror io.Writer
	err error
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	take, kill := w.budget.allow(len(p))
	w.mu.Lock()
	_, _ = w.buf.Write(p[:take])
	if w.mirror != nil && w.err == nil && take > 0 {
		n, e := w.mirror.Write(p[:take])
		if e == nil && n != take {
			e = io.ErrShortWrite
		}
		if e != nil {
			w.err = e
		}
	}
	mirrorErr := w.err
	w.mu.Unlock()
	if mirrorErr != nil {
		w.budget.fail()
	}
	if kill != nil {
		kill()
	}
	return len(p), nil
}
func (w *limitedWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

func (w *limitedWriter) mirrorError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (b *outputBudget) fail() {
	b.mu.Lock()
	kill := b.kill
	b.mu.Unlock()
	if kill != nil {
		kill()
	}
}
