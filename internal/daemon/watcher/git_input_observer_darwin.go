//go:build darwin

package watcher

import (
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"
)

const gitInputPollInterval = 500 * time.Millisecond

// exactInputObserver polls only the declared git config/exclude inputs. Unlike
// kqueue directory watches, its resource use is bounded by the input cap and is
// independent of how many unrelated entries share an input's parent directory.
type exactInputObserver struct {
	paths  []string
	states map[string]exactInputState
	ticker *time.Ticker
}

type exactInputState struct {
	root       string
	info       os.FileInfo
	changeTime time.Time
	err        string
}

func newExactInputObserver(paths []string, interval time.Duration) (*exactInputObserver, error) {
	unique := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path != "" {
			unique[resolveObservedInputPath(path)] = true
		}
	}
	if len(unique) > deadSubtreeMaxObservedFiles {
		return nil, fmt.Errorf("exact git input observer cap exceeded: %d > %d", len(unique), deadSubtreeMaxObservedFiles)
	}
	if interval <= 0 {
		return nil, fmt.Errorf("exact git input observer interval must be positive")
	}

	o := &exactInputObserver{
		paths:  make([]string, 0, len(unique)),
		states: make(map[string]exactInputState, len(unique)),
		ticker: time.NewTicker(interval),
	}
	for path := range unique {
		o.paths = append(o.paths, path)
		o.states[path] = snapshotExactInput(path)
	}
	sort.Strings(o.paths)
	return o, nil
}

func snapshotExactInput(path string) exactInputState {
	state := exactInputState{root: observationRoot(path)}
	info, err := os.Lstat(path)
	if err != nil {
		state.err = err.Error()
		return state
	}
	state.info = info
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		state.changeTime = time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec)
	}
	return state
}

func exactInputStateEqual(a, b exactInputState) bool {
	if a.root != b.root || a.err != b.err || (a.info == nil) != (b.info == nil) {
		return false
	}
	if a.info == nil {
		return true
	}
	return os.SameFile(a.info, b.info) &&
		a.info.Mode() == b.info.Mode() &&
		a.info.Size() == b.info.Size() &&
		a.info.ModTime() == b.info.ModTime() &&
		a.changeTime == b.changeTime
}

// Poll returns exact targets whose file identity, metadata, existence, or
// nearest existing ancestor changed since the prior sample.
func (o *exactInputObserver) Poll() []string {
	if o == nil {
		return nil
	}
	changed := make([]string, 0)
	for _, path := range o.paths {
		next := snapshotExactInput(path)
		if !exactInputStateEqual(o.states[path], next) {
			changed = append(changed, path)
			o.states[path] = next
		}
	}
	return changed
}

func (o *exactInputObserver) Events() <-chan time.Time {
	if o == nil {
		return nil
	}
	return o.ticker.C
}

func (o *exactInputObserver) ActivePaths() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.paths...)
}

func (o *exactInputObserver) Close() {
	if o != nil && o.ticker != nil {
		o.ticker.Stop()
	}
}
