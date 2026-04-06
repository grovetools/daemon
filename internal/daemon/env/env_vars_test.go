package env

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestResolveConfigEnv_NoEnvBlock(t *testing.T) {
	config := map[string]interface{}{"vars": map[string]interface{}{"foo": "bar"}}
	env, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return os.Environ() unchanged
	if len(env) != len(os.Environ()) {
		t.Errorf("expected %d env vars, got %d", len(os.Environ()), len(env))
	}
}

func TestResolveConfigEnv_StaticValues(t *testing.T) {
	config := map[string]interface{}{
		"env": map[string]interface{}{
			"MY_VAR":    "hello",
			"OTHER_VAR": "world",
		},
	}
	env, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := 0
	for _, e := range env {
		if e == "MY_VAR=hello" || e == "OTHER_VAR=world" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 custom env vars, found %d", found)
	}
}

func TestResolveConfigEnv_CmdValues(t *testing.T) {
	config := map[string]interface{}{
		"env": map[string]interface{}{
			"DYNAMIC_VAR": map[string]interface{}{
				"cmd": "echo secret-value",
			},
		},
	}
	env, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, e := range env {
		if e == "DYNAMIC_VAR=secret-value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected DYNAMIC_VAR=secret-value in resolved env")
	}
}

func TestResolveConfigEnv_CmdFailure(t *testing.T) {
	config := map[string]interface{}{
		"env": map[string]interface{}{
			"BAD_VAR": map[string]interface{}{
				"cmd": "exit 1",
			},
		},
	}
	_, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err == nil {
		t.Fatal("expected error for failing command")
	}
	if !strings.Contains(err.Error(), "BAD_VAR") {
		t.Errorf("error should mention the var name, got: %v", err)
	}
}

func TestResolveConfigEnv_MixedValues(t *testing.T) {
	config := map[string]interface{}{
		"env": map[string]interface{}{
			"STATIC":  "value1",
			"DYNAMIC": map[string]interface{}{"cmd": "echo value2"},
		},
	}
	env, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := 0
	for _, e := range env {
		if e == "STATIC=value1" || e == "DYNAMIC=value2" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 custom env vars, found %d", found)
	}
}

func TestResolveConfigEnv_InvalidType(t *testing.T) {
	config := map[string]interface{}{
		"env": map[string]interface{}{
			"BAD": 42,
		},
	}
	_, err := ResolveConfigEnv(context.Background(), config, "/tmp")
	if err == nil {
		t.Fatal("expected error for invalid type")
	}
}
