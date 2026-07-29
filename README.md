# daemon

A new Grove tool - daemon

## Installation

```bash
grove install daemon
```

## Usage

```bash
daemon --help
```

## Documentation

- [Reacting to grove events](docs/reacting-to-grove-events.md) — the lifecycle
  event bus: `[[daemon.hooks.on_event]]` exec hooks, and the `/api/stream`
  SSE contract (sequence numbers, `?since=` replay, `?types=` filtering).

## Development

### Building

```bash
make build
```

### Testing

```bash
make test
make test-e2e
```

### Linting

```bash
make lint
```

## Contributing

This is a private repository. Please ensure all contributions follow the Grove ecosystem conventions.