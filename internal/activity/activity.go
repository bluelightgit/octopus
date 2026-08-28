// Package activity tracks relay traffic while allowing a maintenance pass to
// briefly exclude new relay requests. It intentionally has no dependency on
// the relay or database packages so both sides can use it without an import
// cycle.
package activity

import (
	"sync"
	"sync/atomic"
	"time"
)

var (
	activeRelayRequests atomic.Int64
	lastRelayActivity   atomic.Int64
	maintenanceGate     sync.RWMutex
)

// BeginRelayRequest marks a relay request as active. The returned function is
// safe to call more than once and keeps the operation small so it does not
// affect streaming requests.
func BeginRelayRequest() func() {
	maintenanceGate.RLock()
	activeRelayRequests.Add(1)
	lastRelayActivity.Store(time.Now().UnixNano())
	maintenanceGate.RUnlock()

	var ended atomic.Bool
	return func() {
		if ended.Swap(true) {
			return
		}
		activeRelayRequests.Add(-1)
		lastRelayActivity.Store(time.Now().UnixNano())
	}
}

// Snapshot returns the active relay count and the last time a relay request
// started or finished. A zero time means this process has not served a relay
// request yet.
func Snapshot() (active int64, lastActivity time.Time) {
	active = activeRelayRequests.Load()
	last := lastRelayActivity.Load()
	if last != 0 {
		lastActivity = time.Unix(0, last)
	}
	return active, lastActivity
}

// TryBeginMaintenance obtains an exclusive gate when no relay request is
// active. New relay requests wait only for the bounded maintenance operation,
// preventing a request from starting immediately after the idle check and
// then contending with SQLite's write lock.
func TryBeginMaintenance() (release func(), ok bool) {
	if !maintenanceGate.TryLock() {
		return nil, false
	}
	if activeRelayRequests.Load() != 0 {
		maintenanceGate.Unlock()
		return nil, false
	}
	return maintenanceGate.Unlock, true
}
