package env

import (
	"context"

	coreenv "github.com/grovetools/core/pkg/env"
)

// ResolveConfigEnv delegates to core/pkg/env.ResolveConfigEnv so daemon
// providers (native, docker, terraform) and client-side tooling share the
// same "env" block semantics.
func ResolveConfigEnv(ctx context.Context, config map[string]interface{}, workDir string) ([]string, error) {
	return coreenv.ResolveConfigEnv(ctx, config, workDir)
}
