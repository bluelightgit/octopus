package activity

import (
	"testing"
	"time"
)

func TestTryBeginMaintenanceSkipsActiveRelayRequest(t *testing.T) {
	end := BeginRelayRequest()
	if _, ok := TryBeginMaintenance(); ok {
		end()
		t.Fatal("maintenance should not start while a relay request is active")
	}
	end()

	release, ok := TryBeginMaintenance()
	if !ok {
		t.Fatal("maintenance should start after the relay request finishes")
	}
	release()
}

func TestSnapshotRecordsRelayActivity(t *testing.T) {
	active, last := Snapshot()
	if active != 0 {
		t.Fatalf("expected no active relay requests before test, got %d", active)
	}

	end := BeginRelayRequest()
	active, last = Snapshot()
	if active != 1 {
		t.Fatalf("expected one active relay request, got %d", active)
	}
	if last.IsZero() || time.Since(last) < 0 {
		t.Fatalf("expected a valid last activity time, got %v", last)
	}
	end()

	active, last = Snapshot()
	if active != 0 {
		t.Fatalf("expected no active relay requests after test, got %d", active)
	}
	if last.IsZero() {
		t.Fatal("expected last activity to remain recorded after request completion")
	}
}
