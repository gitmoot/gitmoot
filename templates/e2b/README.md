# E2B review templates

Each directory holds the Dockerfile of one E2B template. Every image starts
from the same pinned `e2bdev/base` digest, creates the same credential-free
runtime directories under `/home/user/.gitmoot` owned by UID/GID 1000
(`user`), and ends as `user`. Gitmoot uploads the OMP binary and the job's
short-lived gateway identity after a sandbox starts, so no image carries a
credential. Root is used only while the image is built.

| Directory | Template name | Size | Toolchain | Pinned from |
| --- | --- | --- | --- | --- |
| `omp-review` | `gitmoot-omp-go126-2c4g-v1` | 2 vCPU / 4 GiB | Go 1.26.4 | Gitmoot `go.mod` |
| `omp-review-swift` | `gitmoot-omp-review-swift-v1` | 4 vCPU / 8 GiB | Swift 6.1.3 (Debian 12 build), fish, zsh, tcsh, csh, GNU awk | herdrup CI (`container: swift:6.1`, login-shell tests); keephair harness |
| `omp-review-rust` | `gitmoot-omp-review-rust-v1` | 4 vCPU / 8 GiB | Rust 1.96.1 + rustfmt, clippy; Zig 0.16.0; just 1.58.0; cargo-nextest 0.9.146; Bun 1.3.14; git 2.56.0 | herdr `rust-toolchain.toml` and CI |
| `omp-review-node` | `gitmoot-omp-review-node-v1` | 4 vCPU / 8 GiB | Node 20.20.2, pnpm 10.13.1, Playwright 1.60.0 Chromium, ffmpeg, uv 0.12.23 | joltra CI (`node-version: 20`), `packageManager`, `pnpm-lock.yaml` |
| `omp-review-flutter` | `gitmoot-omp-review-flutter-v1` | 2 vCPU / 4 GiB | Flutter 3.41.9 (Dart and test artifacts precached) | among-friends and numbra `.mise.toml` |

Dockerfile `ENV` applies only while the image is built: envd starts sandbox
commands with its own default `PATH`
(`/usr/local/bin:/usr/bin:/bin:/usr/local/games:/usr/games`) and
`HOME=/home/user`. So every tool is installed under `/usr` or linked into
`/usr/local/bin`, and per-user state uses the tool's default location under
UID 1000's home: rustup's `~/.rustup` and `~/.cargo`, and Playwright's
`~/.cache/ms-playwright`. The Flutter SDK writes into its own tree at run
time, so `/opt/flutter` is owned by UID 1000. Dependency caches (Cargo
registry, SwiftPM, pub, pnpm store, uv) stay in the user's home and are filled
by the repository under review. For the same reason no `LANG` reaches a
sandbox command, so it runs in the C locale unless the caller sets one at
sandbox creation or per command; keephair's typecheck harness needs
`LANG=C.UTF-8` (its `grep -P` glyph gate silently matches nothing otherwise).

E2B writes `/etc/resolv.conf` without a trailing newline, which Zig 0.16.0
rejects (`ResolvConfParseFailed`) when `zig build` fetches packages; the Rust
image terminates that line at build time.

The images hold no credentials, so a repository that installs private
dependencies at test time (joltra's Python suite pulls the private
`jerryfane/reframekit` over git) needs GitHub access from the job's gateway.

The toolchain images do not carry Go; a Go repository uses `omp-review`.

## Build or rebuild

Template names are immutable versions: a rebuild with changed contents gets a
new name (`-v2`, ...). Build from `omp-review`, which holds the E2B SDK pin and
the build script, and pass the template directory:

```sh
cd templates/e2b/omp-review
npm ci --ignore-scripts
export E2B_API_KEY="$(cat /path/to/e2b-api-key)"

E2B_TEMPLATE_NAME=gitmoot-omp-review-swift-v1 \
  E2B_CPU_COUNT=4 E2B_MEMORY_MB=8192 E2B_MIN_FREE_DISK_MB=8192 \
  npm run build -- ../omp-review-swift
E2B_TEMPLATE_NAME=gitmoot-omp-review-rust-v1 \
  E2B_CPU_COUNT=4 E2B_MEMORY_MB=8192 E2B_MIN_FREE_DISK_MB=20480 \
  npm run build -- ../omp-review-rust
E2B_TEMPLATE_NAME=gitmoot-omp-review-node-v1 \
  E2B_CPU_COUNT=4 E2B_MEMORY_MB=8192 E2B_MIN_FREE_DISK_MB=16384 \
  npm run build -- ../omp-review-node
E2B_TEMPLATE_NAME=gitmoot-omp-review-flutter-v1 \
  E2B_CPU_COUNT=2 E2B_MEMORY_MB=4096 E2B_MIN_FREE_DISK_MB=8192 \
  npm run build -- ../omp-review-flutter
```

Without a directory argument the script builds `omp-review` itself.
`E2B_MIN_FREE_DISK_MB` (optional) asks E2B to grow the filesystem so builds in
the sandbox have room for target directories and caches; the Rust and Node
values cover herdr's debug `target/` and joltra's Python worker dependencies.
Swift, Rust and Node use 4 vCPU / 8 GiB: at 2 vCPU / 4 GiB the kernel
OOM-killed herdrup's test process (3.8 GB resident, in
`RecoveryExecutorTests`) and joltra's Chromium launch exceeded its 10 s hook
timeout; both pass at 4 vCPU / 8 GiB. Rust uses the same size for
herdr's ~4,700-test build. Flutter passes at 2 vCPU / 4 GiB.

The command prints the template ID and build ID; record both with the chosen
resources before pointing any configuration at the new name. Never put an API
key, GitHub credential, repository data, runtime session or job state in these
directories.

## Bump a toolchain

1. Read the new pin from the target repository: its CI workflow, toolchain file
   (`rust-toolchain.toml`, `.mise.toml`), `packageManager` field or lockfile.
2. Update the `ARG` version in the Dockerfile and its checksum from the
   publisher:
   - Swift: swift.org publishes signatures, not digests; download
     `swift-<v>-RELEASE-debian12.tar.gz` and record its `sha256sum`.
   - Rust: `rustup-init.sha256` next to the rustup archive. The Rust toolchain
     itself is verified by rustup.
   - Zig: `shasum` in `https://ziglang.org/download/index.json`.
   - git (Rust image): `sha256sums.asc` on the kernel.org git mirror.
   - Node: `SHASUMS256.txt` of the release.
   - Flutter: `sha256` in
     `https://storage.googleapis.com/flutter_infra_release/releases/releases_linux.json`.
   - just, cargo-nextest, Bun, uv: the release asset digest
     (`gh api repos/<owner>/<repo>/releases/tags/<tag>`).
   - pnpm and Playwright are exact npm versions; Playwright must match the
     version in the repository's lockfile, or its browser revision will not be
     found.
3. Bump the template name's version suffix, build it, and run the canary
   below before switching any configuration to it. Keep the previous template
   until the replacement is verified; rolling back is pointing back at the old
   name.

A base-image bump changes the `FROM` digest in every Dockerfile here.

## Verify a build

`verify.mjs` starts one sandbox from a template, uploads a checkout archive
(one top-level directory, `.git` included), runs a command in it, kills the
sandbox, and prints timing and a list-price cost estimate as JSON. Command
output streams to stderr.

```sh
git clone --depth 1 https://github.com/jerryfane/herdr.git /tmp/herdr
tar -C /tmp -czf /tmp/herdr.tar.gz herdr
E2B_TEMPLATE_NAME=gitmoot-omp-review-rust-v1 VERIFY_SOURCE=/tmp/herdr.tar.gz \
  VERIFY_COMMAND='cargo fmt --check && just ci-tests "all()"' \
  VERIFY_TIMEOUT_MINUTES=50 node verify.mjs
```

E2B limits a sandbox to one hour, so `VERIFY_TIMEOUT_MINUTES` (default 50)
leaves ten minutes for setup.

The canary checks per template:

| Template | Repository | Command |
| --- | --- | --- |
| swift | jerryfane/herdrup | `swift build && swift test` |
| swift | themartianapp/keephair | `export LANG=C.UTF-8; cd ios/Scripts/typecheck-harness && ./run.sh` |
| rust | jerryfane/herdr | `cargo fmt --check && just ci-tests "all()"` |
| node | jerryfane/joltra | `pnpm install --frozen-lockfile && pnpm --filter @joltra/shared build && pnpm --filter @joltra/mcp-widget build && pnpm --filter @joltra/mcp build && pnpm test` |
| flutter | themartianapp/among-friends, jerryfane/numbra | `cd apps/mobile && flutter pub get && flutter test` |

Repository quirks behind these commands, as of their 2026-10-04 heads:
herdr's `cargo test --locked` runs every binary unit test in one process, and
that process is killed by SIGPIPE (herdr's CLI output path sets SIGPIPE back to
its default action process-wide);
herdr's CI runs the suite under cargo-nextest (`just ci-tests`), one process
per test. joltra's `pnpm test` needs the workspace packages its CI builds
first, and its Python suite fetches the private `reframekit` repository.
keephair's harness invokes `docker run swift:5.9-jammy`; in a sandbox, run its
`swiftc` commands with the image's native toolchain instead.

Afterwards confirm that no sandbox is left running
(`GET https://api.e2b.dev/v2/sandboxes`).
