package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNormalRouteRegistrationIsStructurallyClassified(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	serverFile := filepath.Join(filepath.Dir(testFile), "server.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, serverFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", serverFile, err)
	}

	functions := make(map[string]*ast.FuncDecl)
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			functions[fn.Name.Name] = fn
		}
	}
	register := functions["registerNormalRoutes"]
	construct := functions["normalRouteHandler"]
	if register == nil || construct == nil {
		t.Fatal("normal route constructor/registrar not found")
	}

	registrations := 0
	ast.Inspect(register.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "mux" || sel.Sel.Name == "NewServeMux" {
			t.Errorf("normal registrar accesses raw mux at %s", fset.Position(sel.Pos()))
		}
		if sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
			return true
		}
		receiver, ok := sel.X.(*ast.Ident)
		if !ok || receiver.Name != "routes" {
			t.Errorf("normal route bypasses classified registrar at %s", fset.Position(sel.Pos()))
			return true
		}
		registrations++
		return true
	})
	if registrations != len(daemonRoutes) {
		t.Fatalf("normal registrar has %d registrations, classified degraded inventory has %d", registrations, len(daemonRoutes))
	}

	// The constructor may create and return classifiedMux, but must never
	// reach into its raw ServeMux or register a handler itself. This assertion
	// makes bypassing the inventory a source-visible test failure rather than
	// a silent degraded-mode 404.
	ast.Inspect(construct.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "mux" || sel.Sel.Name == "NewServeMux" || sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc" {
			t.Errorf("normal route constructor accesses raw registration surface at %s", fset.Position(sel.Pos()))
		}
		return true
	})
}

func TestDegradedRouteInventoryOnlyAllowsStatusHandlers(t *testing.T) {
	allowed := map[string]degradedRouteMode{
		"/health":          degradedRouteHealth,
		"/api/config":      degradedRouteConfig,
		"/api/system/boot": degradedRouteBoot,
		"/api/sync/status": degradedRouteSyncStatus,
	}
	for _, route := range daemonRoutes {
		want, live := allowed[route.pattern]
		if live {
			if route.degradedMode != want {
				t.Errorf("%s degraded mode = %d, want %d", route.pattern, route.degradedMode, want)
			}
			delete(allowed, route.pattern)
			continue
		}
		if route.degradedMode != degradedRouteRejected {
			t.Errorf("unexpected live degraded route %s (mode %d)", route.pattern, route.degradedMode)
		}
	}
	if len(allowed) != 0 {
		t.Fatalf("status routes missing from inventory: %v", allowed)
	}
}

func TestNormalMuxRegistrationMatchesDegradedRouteTable(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	sock := shortSocketPath(t)
	s := New(false)
	// Listen constructs the normal mux through classifiedMux. It panics if a
	// normal registration is absent from daemonRoutes or a table entry is not
	// mounted, making that table an exhaustive route contract.
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestConfigDegradedSocketServesStatusAndRejectsSubmissions(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	sock := shortSocketPath(t)
	s := New(false)
	s.SetConfigDegradation("/tmp/config/roots.toml: roots.bad: expected path")
	s.SetBootStatus(nil)
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	client := unixHTTPClient(sock)
	assertConfigError := func(path string, wantStatus int, method, body string, key string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, "http://unix"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s returned non-JSON degradation: %q: %v", path, raw, err)
		}
		nested, ok := got[key].(map[string]any)
		if !ok || nested["code"] != "config_load_failed" || nested["recovery"] != "fix the configuration and restart groved" {
			t.Fatalf("%s missing structured restart-only degradation: %#v", path, got)
		}
		if !strings.Contains(nested["message"].(string), "roots.toml") {
			t.Fatalf("%s lost path-qualified config error: %#v", path, got)
		}
		return got
	}

	health := assertConfigError("/health", http.StatusServiceUnavailable, http.MethodGet, "", "config_error")
	if health["degraded"] != true || health["status"] != "degraded" {
		t.Fatalf("health is not truthful: %#v", health)
	}
	cfg := assertConfigError("/api/config", http.StatusOK, http.MethodGet, "", "config_error")
	if cfg["degraded"] != true {
		t.Fatalf("config status is not degraded: %#v", cfg)
	}
	syncStatus := assertConfigError("/api/sync/status", http.StatusOK, http.MethodGet, "", "config_error")
	if syncStatus["enabled"] != false || syncStatus["degraded"] != true {
		t.Fatalf("sync status implies a pipeline is active: %#v", syncStatus)
	}

	bootReq, err := http.NewRequest(http.MethodGet, "http://unix/api/system/boot", nil)
	if err != nil {
		t.Fatal(err)
	}
	bootResp, err := client.Do(bootReq)
	if err != nil {
		t.Fatalf("GET /api/system/boot: %v", err)
	}
	_ = bootResp.Body.Close()
	if bootResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/system/boot = %d, want 200", bootResp.StatusCode)
	}

	// Exercise every route in the normal daemon inventory over the real Unix
	// socket. The degraded mux must replace every non-status handler — reads as
	// well as mutations — with exactly the same structured 503 response. This
	// includes all work-starting/submission paths without relying on their
	// method checks or missing dependencies to fail incidentally.
	var canonicalError map[string]any
	for _, route := range daemonRoutes {
		if route.degradedMode != degradedRouteRejected {
			continue
		}
		got := assertConfigError(route.pattern, http.StatusServiceUnavailable, http.MethodPost, `{}`, "error")
		errBody := got["error"].(map[string]any)
		if canonicalError == nil {
			canonicalError = errBody
			continue
		}
		if !reflect.DeepEqual(errBody, canonicalError) {
			t.Fatalf("%s returned a different degraded error: got %#v, want %#v", route.pattern, errBody, canonicalError)
		}
	}
}
