# Security Policy

NestNAS stores people's personal files, so security reports are taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Report privately via GitHub's "Report a vulnerability" (Security tab) on the repository, with steps to reproduce. You'll get an acknowledgement as soon as possible; please allow time for a fix before disclosing publicly.

## Threat model

NestNAS is designed for a **home LAN or a private overlay network (Tailscale)**.

- Transit: every non-local connection uses TLS 1.3 (own name-constrained local CA, `internal/tlsutil`). Plain HTTP is bound to 127.0.0.1 only and rejects foreign Host headers. Devices must install the CA certificate once; verify its fingerprint (Security tab) before trusting. **Do not port-forward NestNAS to the public internet**; use Tailscale for remote access.
- Device setup: a plain-HTTP page (port 8081) serves only the public CA certificate/profile so new phones can bootstrap trust. It shows the certificate fingerprint to compare, since plain HTTP can be tampered with on a hostile network; use it only on a trusted network. Nothing private is reachable on that port.
- At rest: file contents are encrypted with AES-256-GCM (per-file keys, chunked, authenticated), master key wrapped by a scrypt-derived passphrase key and a recovery key (`internal/vault`). Names, sizes and dates are not encrypted. The format has not been independently audited.
- Passwords are PBKDF2-HMAC-SHA256 (200k iterations, per-user salt). Login attempts are rate-limited per IP.
- Each user is confined to their own folder, including against symlink escapes. Cross-site requests are blocked by an Origin check and `SameSite=Strict` cookies.
- User-hosted websites are sandboxed with a CSP so they cannot reach the admin API.
- Share links are unguessable (128-bit) bearer tokens: anyone holding the link can download that file until it expires or is revoked.
- WebDAV (`/dav/`) uses HTTP Basic authentication, which is safe here because it is only offered over HTTPS to other devices.
- Apps run as Docker containers with the Docker daemon's normal privileges; only install apps you trust.

## In scope

Cryptographic flaws in `internal/vault` or `internal/tlsutil` (key handling, nonce reuse, downgrade, certificate issuance), plus: Path traversal / jail escapes, authentication or authorization bypass, CSRF/XSS, quota bypass, device-setup port exposure (anything beyond the public certificate), insecure defaults.

## Out of scope

Attacks requiring physical access or an already-compromised admin account, issues caused by exposing NestNAS directly to the internet, vulnerabilities in third-party apps installed through the app catalog.

## Supported versions

Only the latest release receives security fixes while the project is pre-1.0.
