package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAllUpdateTypesCoversEveryConstant keeps allUpdateTypes honest against
// the constant block it mirrors. A hand-maintained roster that silently misses
// a type would reintroduce exactly the failure this bus is fixing: an event
// nobody can name, filter, or hook.
func TestAllUpdateTypesCoversEveryConstant(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}

	declared := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "UpdateType" {
				continue
			}
			for _, name := range vs.Names {
				declared[name.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no UpdateType constants — the parser or the file layout changed")
	}

	// Resolve the roster back to constant names by value.
	byValue := map[UpdateType]bool{}
	for _, typ := range allUpdateTypes {
		if byValue[typ] {
			t.Errorf("allUpdateTypes lists %q twice", typ)
		}
		byValue[typ] = true
	}

	// Values are the source of truth on both sides: map each declared constant
	// name to its value via the roster's membership test.
	missing := []string{}
	for name := range declared {
		if !byValue[constValueByName(t, file, name)] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("allUpdateTypes is missing %d constant(s): %v", len(missing), missing)
	}

	if len(byValue) != len(declared) {
		t.Errorf("allUpdateTypes has %d entries but %d constants are declared", len(byValue), len(declared))
	}
}

// constValueByName reads the string literal a named UpdateType constant is
// assigned in the parsed file.
func constValueByName(t *testing.T, file *ast.File, name string) UpdateType {
	t.Helper()
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Strip the surrounding quotes.
				return UpdateType(lit.Value[1 : len(lit.Value)-1])
			}
		}
	}
	t.Fatalf("no string value found for constant %s", name)
	return ""
}

func TestAllUpdateTypesIsSortedAndKnown(t *testing.T) {
	sorted := AllUpdateTypes()
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] >= sorted[i] {
			t.Fatalf("AllUpdateTypes not sorted at %d: %q then %q", i, sorted[i-1], sorted[i])
		}
	}
	for _, typ := range sorted {
		if !IsKnownUpdateType(typ) {
			t.Errorf("IsKnownUpdateType(%q) = false for a listed type", typ)
		}
	}
	if IsKnownUpdateType("definitely_not_a_type") {
		t.Error("IsKnownUpdateType accepted an invented type")
	}
	// Mutating the returned slice must not corrupt the roster.
	sorted[0] = "clobbered"
	if IsKnownUpdateType("clobbered") {
		t.Error("AllUpdateTypes handed out the backing array")
	}
}
