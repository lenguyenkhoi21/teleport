# Build Guide — Teleport v18.10.3 (Keycloak OIDC Patch)

> **Branch**: `patch/v18.10.3-keycloak`
> **Version**: 18.10.3
> **Target**: Linux x86_64 (amd64)
> **Build type**: OSS (Community) — no enterprise code (`e/` directory is empty)

---

## 1. Project Architecture Overview

```
teleport/
├── api/                    # Public API types (separate go module)
├── lib/                    # Core library code
│   ├── auth/               # Authentication server (OIDC patch is here)
│   ├── web/                # Web proxy handlers
│   ├── services/           # Backend services
│   └── ...
├── tool/                   # CLI binaries
│   ├── teleport/           # → binary `teleport` (auth+proxy+node server)
│   ├── tctl/               # → binary `tctl` (admin CLI)
│   ├── tsh/                # → binary `tsh` (user CLI)
│   ├── tbot/               # → binary `tbot` (machine identity)
│   └── teleport-update/    # → binary `teleport-update` (auto-updater)
├── web/                    # Web UI (React/TypeScript)
├── e/                      # Enterprise code (EMPTY in this repo)
├── build.assets/           # Dockerfiles, build scripts
├── Makefile                # Main build system
├── go.mod                  # Go 1.25.12
├── Cargo.toml              # Rust workspace (RDP client)
└── rust-toolchain.toml     # Rust 1.94.0
```

### Binaries produced

| Binary | Description | CGO | Special dependencies |
|--------|-------------|-----|----------------------|
| `teleport` | Auth/Proxy/Node server | ✅ Required | webassets, (optional: BPF, PAM, RDP) |
| `tctl` | Admin CLI | ✅ (for libfido2) | libfido2 (optional) |
| `tsh` | User login CLI | ✅ (for libfido2) | libfido2 (optional) |
| `tbot` | Machine identity | ❌ CGO_ENABLED=0 | None |
| `teleport-update` | Auto-updater | ❌ CGO_ENABLED=0 | None |

---

## 2. System Requirements

### 2.1 Toolchain versions (from source code)

| Tool | Version | Source |
|------|---------|--------|
| Go | **1.25.12** | `go.mod` line 3 |
| Rust | **1.94.0** | `rust-toolchain.toml` |
| Node.js | **24.18.0** | `build.assets/versions.mk` |
| pnpm | (via corepack) | `package.json` |

### 2.2 Install dependencies on Linux (Ubuntu/Debian)

```bash
# Build essentials
sudo apt-get update
sudo apt-get install -y build-essential pkg-config git curl

# Go 1.25.12
wget https://go.dev/dl/go1.25.12.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.25.12.linux-amd64.tar.gz
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

# Rust 1.94.0
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
source "$HOME/.cargo/env"
# rust-toolchain.toml will auto-pin the version during build

# Node.js 24.x (for web UI)
curl -fsSL https://deb.nodesource.com/setup_24.x | sudo -E bash -
sudo apt-get install -y nodejs
corepack enable pnpm

# libfido2 (optional — for MFA hardware key support)
sudo apt-get install -y libfido2-dev

# PAM (optional — for PAM authentication)
sudo apt-get install -y libpam0g-dev

# BPF (optional — for enhanced session recording, requires kernel headers)
sudo apt-get install -y clang llvm libelf-dev
```

### 2.3 Install on macOS (cross-compile for linux-amd64)

```bash
brew install go node corepack pkg-config
corepack enable pnpm

# Rust
brew install rustup && rustup-init -y

# For cross-compiling to linux, use the Docker buildbox (see section 5)
```

---

## 3. Basic Build (Dev mode)

### 3.1 Build all binaries (WITHOUT web UI)

```bash
# Fastest — skip web UI, build Go binaries only
WEBASSETS_SKIP_BUILD=1 make all OS=linux ARCH=amd64
```

Output: `build/teleport`, `build/tctl`, `build/tsh`, `build/tbot`, `build/teleport-update`

### 3.2 Build individual binaries

```bash
# Build teleport server only (skip webassets)
WEBASSETS_SKIP_BUILD=1 make build/teleport OS=linux ARCH=amd64

# Build tctl only
make build/tctl OS=linux ARCH=amd64

# Build tsh only
make build/tsh OS=linux ARCH=amd64

# Build tbot only (no CGO needed)
make build/tbot OS=linux ARCH=amd64
```

### 3.3 Build with web UI (production)

```bash
# Full build: web UI + all binaries
make full OS=linux ARCH=amd64
```

> ⚠️ `make full` builds the web UI (React app) first, requires Node.js + pnpm.
> The web UI is embedded into the `teleport` binary via go:embed.

### 3.4 Debug build

```bash
# Build with debug symbols (for dlv debugger)
TELEPORT_DEBUG=true WEBASSETS_SKIP_BUILD=1 make all OS=linux ARCH=amd64
```

---

## 4. Direct `go build` (without Make)

If you only need to quickly build a single binary without using the Makefile:

```bash
# teleport server (needs webassets or use noembed tag)
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -tags "webassets_embed" -o build/teleport ./tool/teleport

# Without webassets, use a different tag to skip embed:
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/teleport ./tool/teleport

# tctl
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/tctl ./tool/tctl

# tsh
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/tsh ./tool/tsh

# tbot (no CGO needed)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o build/tbot ./tool/tbot
```

> ⚠️ When building `teleport` directly with `go build`, if the web UI hasn't been built,
> the binary will serve an empty web UI. Run `WEBASSETS_SKIP_BUILD=1 make ensure-webassets` to create a placeholder.

---

## 5. Docker Build (Buildbox)

The project provides a Docker buildbox for reproducible builds. Dockerfile is at `build.assets/Dockerfile`.

### 5.1 Build the buildbox image

```bash
# Build buildbox (Ubuntu 22.04 based, all dependencies included)
cd build.assets
make build

# Or build manually
docker build -t teleport-buildbox:latest -f Dockerfile .
```

### 5.2 Build inside the buildbox

```bash
# Run make inside the container
docker run --rm -v $(pwd):/go/src/github.com/gravitational/teleport \
  -w /go/src/github.com/gravitational/teleport \
  teleport-buildbox:latest \
  make full OS=linux ARCH=amd64
```

---

## 6. Create a release tarball

```bash
# Build + package into tarball
make release OS=linux ARCH=amd64

# Output: build/artifacts/teleport-v18.10.3-linux-amd64-bin.tar.gz
```

The tarball contains: `teleport`, `tctl`, `tsh`, `tbot`, `teleport-update`, `fdpass-teleport`, `README.md`, `CHANGELOG.md`, `install`, `examples/`

---

## 7. Build Web UI separately

```bash
# Install dependencies
pnpm install --frozen-lockfile

# Build web UI
make build-ui

# Or manually:
cd web
pnpm build
```

The web UI build output is placed in `webassets/` and embedded into the `teleport` binary during build.

---

## 8. Run Tests

```bash
# Unit tests for the custom OIDC patch
go test ./lib/auth/ -run TestCustomOIDC -v -count=1
go test ./lib/auth/ -run TestMatchOIDCClaims -v -count=1
go test ./lib/auth/ -run TestClaimsToTraitsMapping -v -count=1
go test ./lib/auth/ -run TestPickOIDCUsername -v -count=1

# Full auth package tests (heavy, takes time)
go test ./lib/auth/... -count=1

# API types tests
go test ./api/types/... -count=1
```

---

## 9. Build Flags and Options

### Important environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OS` | auto-detect | Target OS: `linux`, `darwin`, `windows` |
| `ARCH` | auto-detect | Target arch: `amd64`, `arm64`, `arm` |
| `WEBASSETS_SKIP_BUILD` | `0` | Set to `1` to skip building the web UI |
| `TELEPORT_DEBUG` | `false` | Set to `true` for debug builds (with symbols) |
| `FIPS` | (empty) | Set to non-empty for FIPS builds |
| `FIDO2` | auto-detect | `dynamic`, `static`, `yes` for libfido2 support |
| `RDPCLIENT_SKIP_BUILD` | `0` | Set to `1` to skip RDP client (requires Rust) |

### Build tags

| Tag | When to use |
|-----|-------------|
| `webassets_embed` | Embed web UI into binary (production) |
| `pam` | PAM authentication support |
| `bpf` | BPF enhanced session recording (Linux only) |
| `desktop_access_rdp` | Windows Remote Desktop support |
| `libfido2` | Hardware MFA key support |
| `fips` | FIPS 140-2 compliance |

---

## 10. Quick Start — Fastest Build

If you just want to build quickly to test the OIDC patch:

```bash
# 1. Ensure Go 1.25.12+ is installed
go version

# 2. Build teleport + tctl (skip web UI, skip RDP)
WEBASSETS_SKIP_BUILD=1 RDPCLIENT_SKIP_BUILD=1 \
  make build/teleport build/tctl OS=linux ARCH=amd64

# 3. Verify
./build/teleport version
./build/tctl version
```

Total build time: ~2-5 minutes (depending on machine), no Node.js or Rust needed.

---

## 11. Output Structure

```
build/
├── teleport           # Auth + Proxy + SSH Node server (~200MB)
├── tctl               # Admin CLI (~180MB)
├── tsh                # User login CLI (~180MB)
├── tbot               # Machine identity (~50MB, static)
├── teleport-update    # Auto-updater (~30MB, static)
└── fdpass-teleport    # FD passing helper (Rust binary)

build/artifacts/       # Release tarballs (after `make release`)
└── teleport-v18.10.3-linux-amd64-bin.tar.gz
```

> 💡 Binary sizes are large due to Go static linking. Use `make full` (with `-ldflags '-w -s'`) to strip debug info, reducing size by ~30%.
