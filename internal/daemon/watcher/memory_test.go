package watcher

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWarnDeduperFirstOccurrenceLogs(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()

	logNow, suppressed := d.shouldLog("delete stale chunk vectors: database is locked", now)
	if !logNow || suppressed != 0 {
		t.Fatalf("first occurrence: got (logNow=%v, suppressed=%d), want (true, 0)", logNow, suppressed)
	}

	// A different error string is its own key and logs immediately.
	logNow, suppressed = d.shouldLog("insert document: disk I/O error", now)
	if !logNow || suppressed != 0 {
		t.Fatalf("distinct key: got (logNow=%v, suppressed=%d), want (true, 0)", logNow, suppressed)
	}
}

func TestWarnDeduperSuppressesWithinInterval(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()
	key := "database is locked"

	d.shouldLog(key, now)
	for i := 1; i <= 10; i++ {
		logNow, suppressed := d.shouldLog(key, now.Add(time.Duration(i)*time.Second))
		if logNow || suppressed != 0 {
			t.Fatalf("repeat %d within interval: got (logNow=%v, suppressed=%d), want (false, 0)", i, logNow, suppressed)
		}
	}
}

func TestWarnDeduperFlushesSummaryAfterInterval(t *testing.T) {
	d := newWarnDeduper(5 * time.Minute)
	now := time.Now()
	key := "database is locked"

	d.shouldLog(key, now)
	for i := 0; i < 7; i++ {
		d.shouldLog(key, now.Add(time.Minute))
	}

	logNow, suppressed := d.shouldLog(key, now.Add(5*time.Minute))
	if !logNow || suppressed != 7 {
		t.Fatalf("after interval: got (logNow=%v, suppressed=%d), want (true, 7)", logNow, suppressed)
	}

	// The flush resets the window and the counter.
	logNow, suppressed = d.shouldLog(key, now.Add(5*time.Minute+time.Second))
	if logNow || suppressed != 0 {
		t.Fatalf("repeat after flush: got (logNow=%v, suppressed=%d), want (false, 0)", logNow, suppressed)
	}
	logNow, suppressed = d.shouldLog(key, now.Add(10*time.Minute+time.Second))
	if !logNow || suppressed != 1 {
		t.Fatalf("second flush: got (logNow=%v, suppressed=%d), want (true, 1)", logNow, suppressed)
	}
}

func TestIsDatabaseBusyErr(t *testing.T) {
	busy := []error{
		fmt.Errorf("delete stale chunk vectors: %w", errors.New("database is locked")),
		errors.New("database table is locked"),
		errors.New("SQLITE_BUSY: cannot start a transaction within a transaction"),
	}
	for _, err := range busy {
		if !isDatabaseBusyErr(err) {
			t.Errorf("expected busy: %v", err)
		}
	}

	notBusy := []error{
		nil,
		errors.New("disk I/O error"),
		errors.New("no such table: documents"),
	}
	for _, err := range notBusy {
		if isDatabaseBusyErr(err) {
			t.Errorf("expected not busy: %v", err)
		}
	}
}
