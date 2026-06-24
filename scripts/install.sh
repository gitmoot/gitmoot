#!/bin/sh
set -eu

repo="jerryfane/gitmoot"
api_url="https://api.github.com/repos/${repo}/releases?per_page=20"
install_dir="${GITMOOT_INSTALL_DIR:-"$HOME/.local/bin"}"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "gitmoot install: missing required command: $1" >&2
    exit 1
  fi
}

need curl
need sed
need uname

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"

case "$os" in
  linux) os="linux" ;;
  darwin) os="darwin" ;;
  *)
    echo "gitmoot install: unsupported OS: $os" >&2
    exit 1
    ;;
esac

case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "gitmoot install: unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

asset="gitmoot_${os}_${arch}"
case "$asset" in
  gitmoot_linux_amd64|gitmoot_linux_arm64|gitmoot_darwin_amd64|gitmoot_darwin_arm64) ;;
  *)
    echo "gitmoot install: no published beta asset for ${os}/${arch}" >&2
    echo "See https://github.com/${repo}/releases" >&2
    exit 1
    ;;
esac

tag="${GITMOOT_VERSION:-}"
if [ -z "$tag" ]; then
  tag="$(curl -fsSL "$api_url" | sed -n 's/.*"tag_name": "\([^"]*\)".*/\1/p' | head -n 1)"
fi

if [ -z "$tag" ]; then
  echo "gitmoot install: could not find a GitHub release for ${repo}" >&2
  exit 1
fi

base_url="https://github.com/${repo}/releases/download/${tag}"
tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t gitmoot-install)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

echo "Installing Gitmoot ${tag} for ${os}/${arch}"
curl -fL "${base_url}/${asset}" -o "${tmp_dir}/gitmoot"
curl -fL "${base_url}/sha256sums.txt" -o "${tmp_dir}/sha256sums.txt"

expected="$(sed -n "s/  ${asset}$//p" "${tmp_dir}/sha256sums.txt" | head -n 1)"
if [ -z "$expected" ]; then
  echo "gitmoot install: checksum for ${asset} was not found" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp_dir}/gitmoot" | sed 's/ .*//')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${tmp_dir}/gitmoot" | sed 's/ .*//')"
else
  echo "gitmoot install: missing sha256sum or shasum for checksum verification" >&2
  exit 1
fi

if [ "$actual" != "$expected" ]; then
  echo "gitmoot install: checksum mismatch for ${asset}" >&2
  exit 1
fi

mkdir -p "$install_dir"
chmod 755 "${tmp_dir}/gitmoot"
cp "${tmp_dir}/gitmoot" "${install_dir}/gitmoot"
chmod 755 "${install_dir}/gitmoot"

echo "Gitmoot installed to ${install_dir}/gitmoot"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add ${install_dir} to PATH to run gitmoot from any shell." ;;
esac
"${install_dir}/gitmoot" version
