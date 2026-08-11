package watcher

import (
	"testing"

	"github.com/grovetools/core/config"
)

func TestNotespaceRootsAuthorizesImmutableIDThroughDisplaySubscription(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	h := &SyncHandler{
		syncCfg: &config.SyncConfig{Workspaces: []config.SyncWorkspace{{Name: "default"}}},
		watchedPaths: map[string]*syncWatch{
			"/notes/default": {displayName: "default", notespace: id, root: "/notes/default"},
		},
	}
	roots, err := h.NotespaceRoots([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	if roots[id] != "/notes/default" {
		t.Fatalf("root = %q, want /notes/default", roots[id])
	}
	if _, err := h.NotespaceRoots([]string{"01ARZ3NDEKTSV4RRFFQ69G5FAW"}); err == nil {
		t.Fatal("unknown immutable id was accepted")
	}
}
