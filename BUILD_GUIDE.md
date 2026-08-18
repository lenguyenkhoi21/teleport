# Hướng dẫn Build Teleport v18.10.3 (Keycloak OIDC Patch)

> **Branch**: `patch/v18.10.3-keycloak`
> **Version**: 18.10.3
> **Target**: Linux x86_64 (amd64)
> **Build type**: OSS (Community) — không có enterprise code (`e/` directory rỗng)

---

## 1. Tổng quan kiến trúc dự án

```
teleport/
├── api/                    # Public API types (go module riêng)
├── lib/                    # Core library code
│   ├── auth/               # Authentication server (OIDC patch ở đây)
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
├── e/                      # Enterprise code (RỖNG trong repo này)
├── build.assets/           # Dockerfiles, build scripts
├── Makefile                # Build system chính
├── go.mod                  # Go 1.25.12
├── Cargo.toml              # Rust workspace (RDP client)
└── rust-toolchain.toml     # Rust 1.94.0
```

### Các binary được build

| Binary | Mô tả | CGO | Phụ thuộc đặc biệt |
|--------|--------|-----|---------------------|
| `teleport` | Auth/Proxy/Node server | ✅ Bắt buộc | webassets, (tùy chọn: BPF, PAM, RDP) |
| `tctl` | Admin CLI | ✅ (cho libfido2) | libfido2 (tùy chọn) |
| `tsh` | User login CLI | ✅ (cho libfido2) | libfido2 (tùy chọn) |
| `tbot` | Machine identity | ❌ CGO_ENABLED=0 | Không |
| `teleport-update` | Auto-updater | ❌ CGO_ENABLED=0 | Không |

---

## 2. Yêu cầu hệ thống

### 2.1 Toolchain versions (từ source code)

| Tool | Version | Source |
|------|---------|--------|
| Go | **1.25.12** | `go.mod` line 3 |
| Rust | **1.94.0** | `rust-toolchain.toml` |
| Node.js | **24.18.0** | `build.assets/versions.mk` |
| pnpm | (qua corepack) | `package.json` |

### 2.2 Cài đặt dependencies trên Linux (Ubuntu/Debian)

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
# rust-toolchain.toml sẽ tự pin version khi build

# Node.js 24.x (cho web UI)
curl -fsSL https://deb.nodesource.com/setup_24.x | sudo -E bash -
sudo apt-get install -y nodejs
corepack enable pnpm

# libfido2 (tùy chọn — cho MFA hardware key support)
sudo apt-get install -y libfido2-dev

# PAM (tùy chọn — cho PAM authentication)
sudo apt-get install -y libpam0g-dev

# BPF (tùy chọn — cho enhanced session recording, cần kernel headers)
sudo apt-get install -y clang llvm libelf-dev
```

### 2.3 Cài đặt trên macOS (cross-compile cho linux-amd64)

```bash
brew install go node corepack pkg-config
corepack enable pnpm

# Rust
brew install rustup && rustup-init -y

# Nếu cross-compile sang linux, cần Docker buildbox (xem mục 5)
```

---

## 3. Build cơ bản (Dev mode)

### 3.1 Build tất cả binaries (KHÔNG có web UI)

```bash
# Nhanh nhất — skip web UI, chỉ build Go binaries
WEBASSETS_SKIP_BUILD=1 make all OS=linux ARCH=amd64
```

Output: `build/teleport`, `build/tctl`, `build/tsh`, `build/tbot`, `build/teleport-update`

### 3.2 Build từng binary riêng lẻ

```bash
# Chỉ build teleport server (skip webassets)
WEBASSETS_SKIP_BUILD=1 make build/teleport OS=linux ARCH=amd64

# Chỉ build tctl
make build/tctl OS=linux ARCH=amd64

# Chỉ build tsh
make build/tsh OS=linux ARCH=amd64

# Chỉ build tbot (không cần CGO)
make build/tbot OS=linux ARCH=amd64
```

### 3.3 Build có web UI (production)

```bash
# Build full: web UI + all binaries
make full OS=linux ARCH=amd64
```

> ⚠️ `make full` sẽ build cả web UI (React app) trước, cần Node.js + pnpm.
> Web UI được embed vào binary `teleport` qua go:embed.

### 3.4 Build debug mode

```bash
# Build với debug symbols (cho dlv debugger)
TELEPORT_DEBUG=true WEBASSETS_SKIP_BUILD=1 make all OS=linux ARCH=amd64
```

---

## 4. Build trực tiếp bằng `go build` (không qua Make)

Nếu chỉ cần build nhanh 1 binary mà không muốn dùng Makefile:

```bash
# teleport server (cần webassets hoặc dùng noembed tag)
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -tags "webassets_embed" -o build/teleport ./tool/teleport

# Nếu KHÔNG có webassets, dùng tag khác để skip embed:
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/teleport ./tool/teleport

# tctl
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/tctl ./tool/tctl

# tsh
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o build/tsh ./tool/tsh

# tbot (không cần CGO)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o build/tbot ./tool/tbot
```

> ⚠️ Khi build `teleport` bằng `go build` trực tiếp, nếu chưa build web UI thì binary sẽ
> serve web UI trống. Dùng `WEBASSETS_SKIP_BUILD=1 make ensure-webassets` để tạo placeholder.

---

## 5. Build bằng Docker (Buildbox)

Dự án cung cấp Docker buildbox cho reproducible builds. Dockerfile ở `build.assets/Dockerfile`.

### 5.1 Build buildbox image

```bash
# Build buildbox (Ubuntu 22.04 based, có đầy đủ dependencies)
cd build.assets
make build

# Hoặc build thủ công
docker build -t teleport-buildbox:latest -f Dockerfile .
```

### 5.2 Build trong buildbox

```bash
# Chạy make trong container
docker run --rm -v $(pwd):/go/src/github.com/gravitational/teleport \
  -w /go/src/github.com/gravitational/teleport \
  teleport-buildbox:latest \
  make full OS=linux ARCH=amd64
```

---

## 6. Tạo release tarball

```bash
# Build + đóng gói tarball
make release OS=linux ARCH=amd64

# Output: build/artifacts/teleport-v18.10.3-linux-amd64-bin.tar.gz
```

Tarball chứa: `teleport`, `tctl`, `tsh`, `tbot`, `teleport-update`, `fdpass-teleport`, `README.md`, `CHANGELOG.md`, `install`, `examples/`

---

## 7. Build Web UI riêng

```bash
# Cài dependencies
pnpm install --frozen-lockfile

# Build web UI
make build-ui

# Hoặc thủ công:
cd web
pnpm build
```

Web UI build output được đặt vào `webassets/` và embed vào binary `teleport` khi build.

---

## 8. Chạy tests

```bash
# Unit tests cho phần OIDC custom (patch của mình)
go test ./lib/auth/ -run TestCustomOIDC -v -count=1
go test ./lib/auth/ -run TestMatchOIDCClaims -v -count=1
go test ./lib/auth/ -run TestClaimsToTraitsMapping -v -count=1
go test ./lib/auth/ -run TestPickOIDCUsername -v -count=1

# Test toàn bộ auth package (nặng, cần thời gian)
go test ./lib/auth/... -count=1

# Test api types
go test ./api/types/... -count=1
```

---

## 9. Build flags và tùy chọn

### Biến môi trường quan trọng

| Biến | Mặc định | Mô tả |
|------|----------|-------|
| `OS` | auto-detect | Target OS: `linux`, `darwin`, `windows` |
| `ARCH` | auto-detect | Target arch: `amd64`, `arm64`, `arm` |
| `WEBASSETS_SKIP_BUILD` | `0` | Set `1` để skip build web UI |
| `TELEPORT_DEBUG` | `false` | Set `true` cho debug build (có symbols) |
| `FIPS` | (empty) | Set non-empty cho FIPS build |
| `FIDO2` | auto-detect | `dynamic`, `static`, `yes` cho libfido2 support |
| `RDPCLIENT_SKIP_BUILD` | `0` | Set `1` để skip RDP client (cần Rust) |

### Build tags

| Tag | Khi nào dùng |
|-----|-------------|
| `webassets_embed` | Embed web UI vào binary (production) |
| `pam` | PAM authentication support |
| `bpf` | BPF enhanced session recording (Linux only) |
| `desktop_access_rdp` | Windows Remote Desktop support |
| `libfido2` | Hardware MFA key support |
| `fips` | FIPS 140-2 compliance |

---

## 10. Quick Start — Build nhanh nhất

Nếu chỉ muốn build nhanh để test OIDC patch:

```bash
# 1. Đảm bảo Go 1.25.12+ đã cài
go version

# 2. Build teleport + tctl (skip web UI, skip RDP)
WEBASSETS_SKIP_BUILD=1 RDPCLIENT_SKIP_BUILD=1 \
  make build/teleport build/tctl OS=linux ARCH=amd64

# 3. Verify
./build/teleport version
./build/tctl version
```

Tổng thời gian build: ~2-5 phút (tùy máy), không cần Node.js hay Rust.

---

## 11. Cấu trúc output

```
build/
├── teleport           # Auth + Proxy + SSH Node server (~200MB)
├── tctl               # Admin CLI (~180MB)
├── tsh                # User login CLI (~180MB)
├── tbot               # Machine identity (~50MB, static)
├── teleport-update    # Auto-updater (~30MB, static)
└── fdpass-teleport    # FD passing helper (Rust binary)

build/artifacts/       # Release tarballs (sau `make release`)
└── teleport-v18.10.3-linux-amd64-bin.tar.gz
```

> 💡 Binary sizes lớn vì Go static linking. Dùng `make full` (có `-ldflags '-w -s'`) để strip debug info, giảm ~30%.
