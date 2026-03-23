# Contributing to BotMux

Thank you for your interest in contributing to BotMux!

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/<YOU USERNAME>>/botmux.git`
3. Create a feature branch: `git checkout -b my-feature`
4. Make your changes
5. Build and test: `go build -o botmux . && go test -v ./...`
6. Commit your changes with a descriptive message
7. Push to your fork and open a Pull Request

## Development Setup

```bash
# Build (no CGO required)
go build -o botmux .

# Run in demo mode for development
./botmux -demo

# Run tests
go test -v ./...
```

## Project Structure

This is a monolithic Go application — all source files are in `package main`. See the Architecture section in [README.md](README.md#architecture) for a detailed breakdown.

## Guidelines

- **Keep it simple** — BotMux is a single-binary app. Avoid adding unnecessary dependencies.
- **No CGO** — all dependencies must be pure Go to maintain easy cross-compilation.
- **Test your changes** — add tests for new functionality when possible.
- **Update documentation** — if your change affects usage, update README.md and/or the Mintlify docs.
- **Frontend is vanilla JS** — the SPA in `templates/index.html` uses no frameworks. Keep it that way.
- **i18n** — if you add user-facing strings, add both English and Russian translations to the `i18n` object.

## Reporting Issues

- Use [GitHub Issues](https://github.com/skrashevich/botmux/issues) for bug reports and feature requests.
- Include steps to reproduce, expected vs actual behavior, and your environment (OS, Go version, Docker).

## Code Style

- Follow standard Go conventions (`gofmt`).
- Keep commits focused — one logical change per commit.
- Write clear commit messages describing *why*, not just *what*.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).