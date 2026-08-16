package cmd

import (
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/sessions/health"
)

func TestSessionLeasePolicyConfig(t *testing.T) {
	got := sessionLeasePolicy(&config.Config{Daemon: &config.DaemonConfig{SessionLeases: &config.DaemonSessionLeasesConfig{
		Interactive: "3h", Headless: "bad", TurnBased: "45m",
	}}})
	if got.Interactive != 3*time.Hour || got.Headless != health.DefaultHeadlessLease || got.TurnBased != 45*time.Minute {
		t.Fatalf("policy = %+v", got)
	}
}
