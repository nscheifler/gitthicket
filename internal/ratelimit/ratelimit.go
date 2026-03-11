package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	Start time.Time
	Count int
}

type Decision struct {
	Allowed   bool      `json:"allowed"`
	Limit     int       `json:"limit"`
	Count     int       `json:"count"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

func New() *Limiter {
	return &Limiter{
		windows: make(map[string]*window),
	}
}

func (l *Limiter) Allow(agentID, action string, limit int, now time.Time) Decision {
	if limit <= 0 {
		return Decision{Allowed: false, Limit: limit, Count: 0, Remaining: 0, ResetAt: now}
	}
	windowStart := now.UTC().Truncate(time.Hour)
	resetAt := windowStart.Add(time.Hour)
	key := agentID + ":" + action

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.windows[key]
	if !ok || !entry.Start.Equal(windowStart) {
		entry = &window{Start: windowStart}
		l.windows[key] = entry
	}
	if entry.Count >= limit {
		return Decision{
			Allowed:   false,
			Limit:     limit,
			Count:     entry.Count,
			Remaining: 0,
			ResetAt:   resetAt,
		}
	}

	entry.Count++
	return Decision{
		Allowed:   true,
		Limit:     limit,
		Count:     entry.Count,
		Remaining: limit - entry.Count,
		ResetAt:   resetAt,
	}
}
