package main

import (
	"fmt"
	"sync"
	"time"
)

// LogEntry represents a single log line with timestamp
type LogEntry struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// LogBuffer is a thread-safe ring buffer that captures log output
type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	size    int
	pos     int
	full    bool

	// SSE subscribers
	subsMu sync.Mutex
	subs   map[chan LogEntry]struct{}
}

// NewLogBuffer creates a ring buffer that holds up to `size` log entries
func NewLogBuffer(size int) *LogBuffer {
	return &LogBuffer{
		entries: make([]LogEntry, size),
		size:    size,
		subs:    make(map[chan LogEntry]struct{}),
	}
}

// Write implements io.Writer so it can be used with log.SetOutput
func (lb *LogBuffer) Write(p []byte) (n int, err error) {
	msg := string(p)
	// Strip trailing newline that log package adds
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}

	entry := LogEntry{
		Time:    time.Now(),
		Message: msg,
	}

	lb.mu.Lock()
	lb.entries[lb.pos] = entry
	lb.pos = (lb.pos + 1) % lb.size
	if lb.pos == 0 {
		lb.full = true
	}
	lb.mu.Unlock()

	// Notify SSE subscribers
	lb.subsMu.Lock()
	for ch := range lb.subs {
		select {
		case ch <- entry:
		default: // drop if subscriber is slow
		}
	}
	lb.subsMu.Unlock()

	return len(p), nil
}

// Recent returns the last `n` log entries in chronological order
func (lb *LogBuffer) Recent(n int) []LogEntry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	count := lb.pos
	if lb.full {
		count = lb.size
	}
	if n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}

	result := make([]LogEntry, n)
	start := lb.pos - n
	if start < 0 {
		start += lb.size
	}
	for i := 0; i < n; i++ {
		result[i] = lb.entries[(start+i)%lb.size]
	}
	return result
}

// Subscribe returns a channel that receives new log entries
func (lb *LogBuffer) Subscribe() chan LogEntry {
	ch := make(chan LogEntry, 64)
	lb.subsMu.Lock()
	lb.subs[ch] = struct{}{}
	lb.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel
func (lb *LogBuffer) Unsubscribe(ch chan LogEntry) {
	lb.subsMu.Lock()
	delete(lb.subs, ch)
	lb.subsMu.Unlock()
	close(ch)
}

// FormatEntry returns a formatted log line
func (e LogEntry) FormatEntry() string {
	return fmt.Sprintf("[%s] %s", e.Time.Format("15:04:05"), e.Message)
}
