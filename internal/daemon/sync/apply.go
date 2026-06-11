package sync

// Package apply provides low-level event application with OCC guards.
// This file is a placeholder for apply-specific logic; most application
// happens in pull.go. Separated here for future expansion (e.g., bulk apply transactions).

// ApplyEvent is the public contract for applying a sync event to the daemon's state.
// It ensures OCC guards (base_version) are respected and that edit-wins-over-delete
// behavior is implemented correctly.
type ApplyEvent interface {
	Apply() error
}
