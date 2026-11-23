# Contributing to NestNAS

Thanks for helping! NestNAS's promise is **"anyone can turn a laptop into a NAS"**, so changes are judged by how well they serve non-technical users, and by safety.

## Setup

```bash
git clone <your fork> && cd nestnas
make test      # vet + unit tests
make smoke     # end-to-end test against a real server
make build && ./nestnas
```

Needs Go 1.18+. No other dependencies, and please keep it that way (see `AGENTS.md` for the hard constraints and security invariants).

## Pull requests

1. Open an issue first for anything bigger than a small fix.
2. Keep PRs focused. Include tests: unit tests for logic, a check in `scripts/smoke.sh` for new endpoints.
3. `gofmt`, `go vet`, `make test`, `make smoke` must pass (CI runs them).
4. User-visible change? Update `README.md`.
5. Be honest in the PR description about what you could and could not test (real Docker, other OSes, real phones).

## Good first areas

- A "forgot password" / reset command, and converting an existing NAS to encrypted storage
- Public-CA HTTPS via Tailscale certificates (so devices don't need a certificate install)
- Automatic phone backup (iOS Shortcuts with device passwords, or a native app)
- Search, file versioning, sharing a folder between users
- Windows / macOS parity (free-disk checks, real-device testing)
- Translations, UI polish and accessibility
- Packaging: `.deb`, Homebrew, winget

## Code of conduct


Be kind and constructive. Assume good intent. No harassment.
