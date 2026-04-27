package watcher

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LanguageProfile defines language-specific behavior for indexing source code.
type LanguageProfile struct {
	Name           string
	Extensions     []string
	ProjectMarkers []string
	ExcludeDirs    []string
	TestPatterns   []string // suffix patterns to skip (e.g., "_test.go", ".spec.ts")
	ExtractMeta    func(content, filePath string) (repo, pkg string, imports []string)
}

// languageRegistry holds all registered language profiles, keyed by name.
var languageRegistry = map[string]*LanguageProfile{
	"go":         goProfile(),
	"rust":       rustProfile(),
	"python":     pythonProfile(),
	"typescript": typescriptProfile(),
	"terraform":  terraformProfile(),
}

// allSupportedExtensions returns a set of all file extensions across profiles.
func allSupportedExtensions() map[string]bool {
	exts := make(map[string]bool)
	for _, p := range languageRegistry {
		for _, ext := range p.Extensions {
			exts[ext] = true
		}
	}
	return exts
}

// allExcludeDirs returns a set of all directories to exclude across profiles.
func allExcludeDirs() map[string]bool {
	dirs := map[string]bool{
		".git":     true,
		"testdata": true,
	}
	for _, p := range languageRegistry {
		for _, d := range p.ExcludeDirs {
			dirs[d] = true
		}
	}
	return dirs
}

// allProjectMarkers returns all project marker filenames across profiles.
func allProjectMarkers() []string {
	seen := make(map[string]bool)
	var markers []string
	for _, p := range languageRegistry {
		for _, m := range p.ProjectMarkers {
			if !seen[m] {
				seen[m] = true
				markers = append(markers, m)
			}
		}
	}
	return markers
}

// profileForExt returns the language profile matching a file extension, or nil.
func profileForExt(ext string) *LanguageProfile {
	for _, p := range languageRegistry {
		for _, e := range p.Extensions {
			if e == ext {
				return p
			}
		}
	}
	return nil
}

// isTestFile checks if a file path matches any test pattern for its language.
func isTestFile(path string, profile *LanguageProfile) bool {
	base := filepath.Base(path)
	for _, pat := range profile.TestPatterns {
		if strings.HasPrefix(pat, "*") {
			if strings.HasSuffix(base, pat[1:]) {
				return true
			}
		} else {
			if strings.HasPrefix(base, pat) {
				return true
			}
		}
	}
	return false
}

// --- Go ---

func goProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:           "go",
		Extensions:     []string{".go"},
		ProjectMarkers: []string{"go.mod"},
		ExcludeDirs:    []string{"vendor", "node_modules", "dist"},
		TestPatterns:   []string{"*_test.go"},
		ExtractMeta: func(content, filePath string) (string, string, []string) {
			pkgName := extractGoPackage(content)
			modName, modRoot := findGoModule(filepath.Dir(filePath))
			imports := extractGoImports(content)
			_ = modRoot // used by caller for relPath
			return modName, pkgName, imports
		},
	}
}

// --- Rust ---

var (
	rustUseRegex   = regexp.MustCompile(`(?m)^use\s+([^;]+);`)
	cargoNameRegex = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)
)

func rustProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:           "rust",
		Extensions:     []string{".rs"},
		ProjectMarkers: []string{"Cargo.toml"},
		ExcludeDirs:    []string{"target"},
		TestPatterns:   []string{}, // Rust tests are inline, not separate files
		ExtractMeta: func(content, filePath string) (string, string, []string) {
			repo := findManifestField(filepath.Dir(filePath), "Cargo.toml", cargoNameRegex)
			pkg := filepath.Base(filepath.Dir(filePath))
			var imports []string
			for _, m := range rustUseRegex.FindAllStringSubmatch(content, -1) {
				imports = append(imports, strings.TrimSpace(m[1]))
			}
			return repo, pkg, imports
		},
	}
}

// --- Python ---

var (
	pyImportRegex      = regexp.MustCompile(`(?m)^(?:from\s+(\S+)\s+)?import\s+(.+)`)
	pyProjectNameRegex = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)
)

func pythonProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:           "python",
		Extensions:     []string{".py"},
		ProjectMarkers: []string{"pyproject.toml"},
		ExcludeDirs:    []string{"__pycache__", ".venv", "venv", ".tox"},
		TestPatterns:   []string{"test_*", "*_test.py", "*.spec.py"},
		ExtractMeta: func(content, filePath string) (string, string, []string) {
			repo := findManifestField(filepath.Dir(filePath), "pyproject.toml", pyProjectNameRegex)
			pkg := filepath.Base(filepath.Dir(filePath))
			var imports []string
			for _, m := range pyImportRegex.FindAllStringSubmatch(content, -1) {
				if m[1] != "" {
					imports = append(imports, m[1])
				} else {
					imports = append(imports, strings.TrimSpace(m[2]))
				}
			}
			return repo, pkg, imports
		},
	}
}

// --- TypeScript/JavaScript ---

var (
	tsImportRegex   = regexp.MustCompile(`(?m)^import\s+.*from\s+['"]([^'"]+)['"]`)
	packageNameJSON = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
)

func typescriptProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:           "typescript",
		Extensions:     []string{".ts", ".tsx", ".js"},
		ProjectMarkers: []string{"package.json", "tsconfig.json"},
		ExcludeDirs:    []string{"node_modules", "dist", "build", ".next"},
		TestPatterns:   []string{"*.spec.ts", "*.test.ts", "*.spec.tsx", "*.test.tsx", "*.spec.js", "*.test.js"},
		ExtractMeta: func(content, filePath string) (string, string, []string) {
			repo := findManifestField(filepath.Dir(filePath), "package.json", packageNameJSON)
			pkg := filepath.Base(filepath.Dir(filePath))
			var imports []string
			for _, m := range tsImportRegex.FindAllStringSubmatch(content, -1) {
				imports = append(imports, m[1])
			}
			return repo, pkg, imports
		},
	}
}

// --- Terraform ---

var tfModuleRegex = regexp.MustCompile(`(?m)source\s*=\s*"([^"]+)"`)

func terraformProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:           "terraform",
		Extensions:     []string{".tf"},
		ProjectMarkers: []string{"main.tf"},
		ExcludeDirs:    []string{".terraform"},
		TestPatterns:   []string{},
		ExtractMeta: func(content, filePath string) (string, string, []string) {
			repo := filepath.Base(filepath.Dir(filePath))
			pkg := ""
			var imports []string
			for _, m := range tfModuleRegex.FindAllStringSubmatch(content, -1) {
				imports = append(imports, m[1])
			}
			return repo, pkg, imports
		},
	}
}

// findManifestField walks up from startDir looking for a manifest file and
// extracts a field using the provided regex. Returns the directory basename as fallback.
func findManifestField(startDir, manifestName string, fieldRegex *regexp.Regexp) string {
	current := startDir
	for {
		manifestPath := filepath.Join(current, manifestName)
		if b, err := os.ReadFile(manifestPath); err == nil { //nolint:gosec // G304: manifest from project tree
			if matches := fieldRegex.FindStringSubmatch(string(b)); len(matches) > 1 {
				return strings.TrimSpace(matches[1])
			}
			return filepath.Base(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return filepath.Base(startDir)
}
