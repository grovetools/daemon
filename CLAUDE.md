# Grove Build Instructions for Claude

This file contains important instructions for Claude when working with this repository.

## Building and Testing

1. **Build using grove**:
   ```bash
   grove build      # Creates binary in ./bin
   ```

2. **Run tests using tend**:
   ```bash
   tend run -p      # Runs end-to-end tests in parallel
   tend list        # See available tests
   ```

3. **Binary Management** - IMPORTANT:
   - Binaries are created in the `./bin` directory
   - **NEVER** copy binaries elsewhere in the PATH
   - Binaries are managed by the `grove` meta-tool
   - Use `grove list` to see currently active binaries across the ecosystem

## Additional Notes

- Always use `make clean` before switching branches or making significant changes
- The version information is injected during build time via LDFLAGS
- For development builds with race detection, use `make dev`

## Looking Up Related Concepts

Before starting work, search for existing concepts that may relate to your task:

```bash
nb concept search "<keyword>" --ecosystem --files-only
nb concept list --ecosystem --json
```

This helps you understand existing architectural decisions and avoid duplicating documentation.

When done with your task, offer to invoke the `/concept-maintainer` skill to update any affected concepts.
