# 🪺 NestNAS

**Turn any laptop into your own private cloud.** Choose how much disk space to lend, and NestNAS gives you a web file manager, photo gallery, phone backup, share links, multiple users, an encrypted network drive and more, with nothing else to install.

Open source (MIT). One program, written in Go using only the standard library. No database.

Website: <https://nestnas.vercel.app> (its source is in [`site/`](site/)).

> **Status: early (pre-1.0).** It works end to end and is well tested, but it has had no independent security audit, and some parts have only been tried on one Linux laptop (see [Known limitations](#known-limitations)). Don't rely on it as your only copy of irreplaceable data.

## Features

| Feature | How it works |
|---|---|
| **Space allowance** | Files live in a normal folder; NestNAS enforces a per-user quota (no partitioning, no root) |
| **Web file manager** | Upload (drag and drop, progress), resumable download, folders, rename, zip a whole folder |
| **Preview and gallery** | Open photos, videos, audio, PDFs and text inside NestNAS: thumbnails, full-screen viewer, swipe/arrow keys, video seeking. Never cached by the browser. HTML/SVG are never previewed (they can run script) |
| **Phone/computer backup** | The **Backup** tab copies photos, videos or a folder to the NAS over Wi-Fi. Safe to repeat: skips what's already there, never overwrites or deletes, keeps each photo's date, saves under `Phone Backup/<device>/<year>/<month>`, retries bad Wi-Fi. Handy for freeing up iCloud space |
| **Encrypted at rest** | Optional (chosen at setup): file contents are AES-256-GCM encrypted before they reach the disk |
| **Encrypted in transit** | HTTPS (TLS 1.3 only) with NestNAS's own private certificate authority |
| **Network drive** | Built-in WebDAV: mount your NAS in Finder, Windows Explorer or Linux file managers, signing in with your NestNAS login |
| **Trash** | Deleting moves to a per-user trash; restore any time; purged after 30 days |
| **Share links** | Public download link per file, with expiry and revoke |
| **Users and quotas** | Admin adds people; each gets a private folder and their own limit |
| **External drives** | Pick a USB/external drive as storage. Unplug and replug whenever you like (see below) |
| **Mobile-friendly** | Bottom tab bar, tap-friendly lists, installable on a phone's Home Screen |
| **Desktop app** | The same dashboard in its own native window (Linux package) |
| **Remote access** | Guided Tailscale setup. No port forwarding |
| **App hosting** | One-click Jellyfin, Vaultwarden, Syncthing, Uptime Kuma via Docker (not available together with encryption for Jellyfin's media library) |
| **Static websites** | Put files in a `site` folder and they're served at `/sites/<username>/` |

## Install

**From source** (needs Go 1.18+; this is the way until the first release is tagged):

```bash
git clone https://github.com/Saransh-Jainbu/nestnas.git
cd nestnas
make build
./nestnas service install      # start now and on every login/boot
```

Then open <http://localhost:8080>. A two-minute setup page asks for an admin login, how many GB to lend, where files live, and whether to encrypt them.

**One-line installers** (verify a published SHA-256 checksum before installing; they work once a release exists):

```bash
curl -fsSL https://raw.githubusercontent.com/Saransh-Jainbu/nestnas/main/install.sh | sh      # Linux / macOS
```
```powershell
irm https://raw.githubusercontent.com/Saransh-Jainbu/nestnas/main/install.ps1 | iex           # Windows
```

### Desktop app (Linux)

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.0-dev     # build dependencies
make desktop-deb && sudo apt install ./dist/nestnas_*.deb
```

It shows the dashboard in a native window and attaches to the background service if one is running. It lives in its own Go module (`desktop/`, Wails v2.6) so the core stays dependency-free.

### Autostart

```bash
nestnas service install      # run in the background now and on every boot/login
nestnas service status
nestnas service uninstall    # removes autostart only; files and settings are untouched
```

Runs as your normal user, no root needed: systemd user unit (Linux), LaunchAgent (macOS), Task Scheduler (Windows). **Laptop tip:** closing the lid suspends the machine and takes the NAS offline; `service install` prints the setting to prevent that.

## Ports

| Port | Who can reach it | What it serves |
|---|---|---|
| `8080` | **This computer only** (`127.0.0.1`) | The dashboard over plain HTTP (never touches the network) |
| `8443` | Other devices on your network | The dashboard, WebDAV and share links over **HTTPS (TLS 1.3)** |
| `8081` | Other devices on your network | **Device setup page: only the public certificate** and an Apple profile, so a new phone can start trusting your NAS |

## Setting up a phone

1. On the phone (same Wi-Fi), open `http://<laptop-ip>:8081` in **Safari** and install the certificate profile it offers. On iPhone/iPad then turn it on under *Settings, General, About, Certificate Trust Settings*. The dashboard's **Security** tab has steps for Android, Windows, Mac and Linux too, and the certificate fingerprint to compare.
2. Open `https://<laptop-ip>:8443`, log in, then Share, **Add to Home Screen** to use it like an app.
3. Use the **Backup** tab to copy your photos and videos to the NAS.

Apple's **Files app does not support WebDAV** (it only speaks `smb://`), so on an iPhone use the web app, or a WebDAV app such as Documents by Readdle with `https://<ip>:8443/dav/`.

## Security design

- **Passwords:** PBKDF2-HMAC-SHA256 (200k iterations), random per-user salt, constant-time compare. Login attempts are rate-limited per IP.
- **Sessions:** random 256-bit tokens in `HttpOnly; SameSite=Strict` (and `Secure` over HTTPS) cookies, CSRF origin check on every write.
- **Files:** every path is resolved inside the user's own folder, **including through symlinks** (`internal/fsx`). Users only see their own folder.
- **In transit:** TLS 1.3 only (`internal/tlsutil`). NestNAS creates a private certificate authority that is **name-constrained to private network addresses**, so even if its key leaked it couldn't be used to impersonate a public website. Certificates are re-issued automatically when the laptop's IP changes.
- **At rest** (`internal/vault`): AES-256-GCM in 64 KiB authenticated chunks, a separate key per file; tampering, truncation and reordering are detected. The master key is wrapped by your passphrase (memory-hard **scrypt**, 64 MiB per guess) and by a one-time **recovery key**. After each restart an admin unlocks the storage (or opts in to auto-unlock, which keeps the key on the laptop's own disk). **File and folder names, sizes and dates are not hidden; contents are.**
- **User websites** are served with `Content-Security-Policy: sandbox` so they can't reach the admin API.
- Don't port-forward NestNAS to the internet; use Tailscale for access away from home.

All the cryptographic primitives that matter (PBKDF2, HKDF, scrypt) are checked against the official RFC test vectors and cross-checked with OpenSSL. See [SECURITY.md](SECURITY.md) for the threat model and how to report a vulnerability.

### Using an external drive

Pick it in the setup page: **Choose folder…** lists your drives with free space and warns about limits (FAT32 can't hold files over 4 GB; NTFS/exFAT don't keep Linux permissions).

**Unplug and replug whenever you like.** NestNAS stamps its storage folder with a hidden identity file and remembers where it sits on the drive:

- **Drive removed:** NestNAS pauses and shows a warning. It never falls back to your laptop's own disk, and it refuses any *other* drive that appears at the same place.
- **Drive plugged back in:** it resumes by itself within seconds, even if your computer mounts it somewhere different this time. Encrypted storage stays unlocked.
- A file mid-upload when the drive is pulled fails and must be re-sent. Docker apps keep data on the drive too, so restart them after replugging.

### Network drive (WebDAV) notes

- Address: `https://<ip>:8443/dav/` with your NestNAS username and password. Install the NestNAS certificate on the device first.
- Tested against GNOME's real WebDAV client (mount, list, create, copy in and out, read back). macOS Finder and Windows are expected to work but **haven't been tested on real devices yet**.
- Deleting over WebDAV goes to the trash. Locks are accepted but not enforced (enough for Finder to write).

## Known limitations

- **Encryption:** names, sizes and dates stay visible. It protects a stolen laptop or drive while locked, and your traffic on the network; it does not protect against malware on the unlocked NAS or someone who knows your passphrase. If you lose both the passphrase and the recovery key, your files are unrecoverable by design. It can only be turned on during setup for now. The file format is NestNAS's own and **has not been independently audited**.
- **Phones:** iOS Safari can't upload in the background, so phone backup needs the page open with the screen on. Truly automatic backup would need a native app.
- **Certificates:** each new device needs a one-time certificate install; there is no public-CA (Let's Encrypt/Tailscale) option yet.
- **Platforms:** developed and tested on Ubuntu Linux. Windows and macOS builds compile and the logic is shared, but they haven't been run for real. Free-disk checks aren't implemented on Windows.
- **Not yet verified on real hardware:** real Docker app installs, Tailscale setup, systemd/launchd/Task Scheduler registration on other OSes, and iPhone/Mac/Windows clients.
- Sessions are in memory (a restart means logging in again). There's no "forgot password" flow yet.

## Project layout

```
main.go                  CLI: serve | service | version
pkg/app                  public, stdlib-only API to embed the server (used by desktop/)
internal/config          users, quotas, share links (JSON file, atomic writes)
internal/auth            password hashing, PBKDF2 / HKDF / scrypt
internal/vault           at-rest file encryption and key management
internal/tlsutil         private certificate authority and TLS 1.3 setup
internal/fsx             symlink-safe path jail, usage accounting
internal/server          HTTP API, WebDAV, previews, backup, trash, apps, sessions
internal/service         autostart: systemd / launchd / Task Scheduler
internal/web             the embedded single-file dashboard
desktop/                 native window (separate Go module, Wails)
install.sh, install.ps1  one-line installers (checksum-verified)
scripts/smoke.sh         end-to-end tests:  make smoke
.github/workflows        CI, and tagged releases (git tag v0.1.0)
```

## Roadmap

1. Turn encryption on for an existing NAS (convert files in place); "forgot password" reset command
2. Public-CA HTTPS via Tailscale certificates (no certificate install on devices)
3. Automatic phone backup (iOS Shortcuts / device passwords, or a native app)
4. Search, file versioning, folder sharing between users
5. Native packages (`.deb`, `.msi`, Homebrew) and auto-update
6. Disk health (SMART), backups to a second disk or cloud (S3/R2), mDNS (`nestnas.local`)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md). Working with AI coding agents: [AGENTS.md](AGENTS.md).

`make test` and `make smoke` must pass (smoke runs twice: encryption off and on). Keep it beginner-friendly: every feature should work from the web UI with zero config files.

## License

MIT, see [LICENSE](LICENSE).
