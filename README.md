# rcpud — Remote CPU Daemon

Connect to Linux machines from 9front using standard `rcpu`.

rcpud is a 9P-native remote shell and graphics daemon. It speaks drawterm's cpu protocol — dp9ik authentication via factotum, TLS-PSK encrypted transport, exportfs namespace export, and wsysmsg draw protocol for graphical sessions. No changes needed on the 9front side. Run `rcpu -h <host>` and it works.

## Quick start

### Prerequisites

- plan9port (for factotum, rc, auth libraries)
- OpenSSL development headers (for TLS-PSK)
- Go 1.21+

### Build

```sh
# rcpud — the CPU daemon itself
go build -tags rcpud -o bin/rcpud .

# drawsrv — framebuffer forwarding for graphical sessions
go build -tags drawsrv -o bin/drawsrv .
```

### Run

```sh
# Start factotum with a dp9ik key
export NAMESPACE=/tmp/ns.myns
factotum &
echo 'key proto=dp9ik dom=example.com user=scott !password=secret' \
  | 9p write factotum/ctl

# Start rcpud
rcpud -l :17019

# Start drawsrv for graphical desktop forwarding
drawsrv -tcp :17029
```

### Connect from 9front

```sh
# Shell session
rcpu -h 192.168.2.2 -u scott

# Graphical session (coming soon)
rcpu -G -h 192.168.2.2 -u scott
```

## Features

### Working

| Feature | Status |
|---------|--------|
| dp9ik authentication via factotum | Done |
| TLS-PSK encrypted channel | Done |
| Interactive rc shell with pty job control | Done |
| Delete key (note relay → SIGINT) | Done |
| `/dev/cons` proxied from drawterm | Done |
| ANSI escape stripping for clean output | Done |
| `-mount` flag: serve local directories | Done |
| Hostname display in shell prompt | Done |
| systemd service file | TBD |

### drawsrv (graphical desktop forwarding)

| Feature | Status |
|---------|--------|
| `/dev/fb0` capture via mmap | Done |
| wsysmsg protocol (drawfcall.h) | Done |
| uinput mouse injection | Done |
| TCP socket (`-tcp :port`) | Done |
| Keyboard injection | TBD |
| Channel format conversion | TBD |
| Framebuffer delta updates | TBD |
| Wire into rcpud namespace | TBD |

## Comparison with lx

[perpen/lx](https://github.com/perpen/lx) is a similar project: a Plan 9 → Linux cpu server written in C with 69 stars. It has the right ideas. Here is the honest comparison.

### What lx does well

- Namespace export from Plan 9 to Linux (Plan 9 files appear under `/9`)
- Note-to-signal translation (Delete key → SIGINT/SIGHUP/SIGKILL)
- X11 forwarding via VNC
- Clean C code with plan9port libraries

### Where rcpud wins

| | rcpud | lx |
|---|---|---|
| **Authentication** | dp9ik via factotum, the standard Plan 9 auth protocol | None |
| **Encryption** | TLS-PSK using dp9ik session key | None |
| **Client** | Unmodified drawterm — `rcpu -h <host>` | Custom `lx` binary must be installed on Plan 9 side |
| **Language** | Go (easy to extend, no bootstrapping) | C (requires plan9port on Linux to compile) |
| **Latency** | Persistent session — one connection per shell | ~0.6s per invocation (runs exportfs+9pfuse each time) |
| **Pty** | Yes — shell gets a real pty for job control | No — "lx does not provide a pty" (README) |
| **Draw protocol** | Native wsysmsg, Plan 9's own draw protocol | VNC only |
| **Maintained** | Active (2026) | Last commit 6 years ago |

### In short

lx is a clever hack from 2019. It proved the concept but was never production-ready. rcpud is the production version: proper auth, encryption, persistent sessions, no client-side tools needed, active development. You just run `rcpu` from your existing 9front system.

## Architecture

```
9front drawterm
    │
    ├── rcpu ──→ rcpud:17019 ──→ dp9ik auth ──→ TLS-PSK ──→ shell + namespace
    │
    └── rcpu -G ──→ rcpud:17019 ──→ auth ──→ drawsrv:17029 ──→ fb capture ──→ wsysmsg
                                                                         │
                                                     uinput mouse/kbd ←──┘
```

## Project structure

```
rcpud.go      — CPU daemon: auth, TLS, pty shell, namespace serving
authproxy.go  — Fixed factotum proxy (io.ReadFull TCP bugfix)
ssl_psk.go    — OpenSSL TLS-PSK wrapper
mount.go      — Local directory serving through 9P namespace
drawsrv.go    — Framebuffer capture + wsysmsg protocol server
drawproxy.go  — drawsrv socket proxying into rcpud namespace (WIP)
gortns.go     — Agent manager (experimental)
```

## License

MIT
