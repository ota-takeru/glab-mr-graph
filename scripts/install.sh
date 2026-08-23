#!/bin/sh
set -eu

error() {
  printf '%s\n' "glab-mr-graph installer: $*" >&2
  exit 1
}

has_command() {
  command -v "$1" >/dev/null 2>&1
}

shell_quote() {
  quoted=$(printf '%s' "$1" | sed "s/'/'\\\\''/g")
  printf "'%s'" "$quoted"
}

alias_command_from_list() {
  printf '%s\n' "$1" | awk '
    $1 == "Alias" { next }
    $1 == "mr-graph" {
      sub(/^[^[:space:]]+[[:space:]]+/, "")
      sub(/^!/, "")
      print
      exit
    }
  '
}

has_command glab || error "glab is required; install GitLab CLI and run glab auth login first"

os_name=$(uname -s 2>/dev/null || printf 'unknown')
case "$os_name" in
  Darwin) release_os=darwin ;;
  Linux) release_os=linux ;;
  FreeBSD) release_os=freebsd ;;
  *) error "unsupported operating system: $os_name (supported: darwin, linux, freebsd)" ;;
esac

machine_arch=$(uname -m 2>/dev/null || printf 'unknown')
case "$machine_arch" in
  amd64|x86_64) release_arch=amd64 ;;
  arm64|aarch64) release_arch=arm64 ;;
  386|i386|i686|x86) release_arch=386 ;;
  arm|armv6*|armv7*) release_arch=arm ;;
  *) error "unsupported architecture: $machine_arch (supported: amd64, arm64, 386, arm)" ;;
esac

asset_name="$release_os-$release_arch"
version=${GLAB_MR_GRAPH_VERSION:-latest}
if [ "$version" = latest ]; then
  download_url="https://github.com/ota-takeru/glab-mr-graph/releases/latest/download/$asset_name"
elif printf '%s\n' "$version" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' >/dev/null 2>&1; then
  download_url="https://github.com/ota-takeru/glab-mr-graph/releases/download/$version/$asset_name"
else
  error "GLAB_MR_GRAPH_VERSION must be latest or a release tag such as v0.1.0"
fi

if [ -n "${GLAB_MR_GRAPH_INSTALL_DIR:-}" ]; then
  requested_install_dir=$GLAB_MR_GRAPH_INSTALL_DIR
elif [ -n "${XDG_BIN_HOME:-}" ]; then
  requested_install_dir=$XDG_BIN_HOME
elif [ -n "${HOME:-}" ]; then
  requested_install_dir=$HOME/.local/bin
else
  error 'set GLAB_MR_GRAPH_INSTALL_DIR when HOME is not available'
fi

mkdir -p "$requested_install_dir" || error "cannot create install directory: $requested_install_dir"
install_dir=$(CDPATH= cd -- "$requested_install_dir" && pwd -P) || error "cannot resolve install directory: $requested_install_dir"
binary_path=$install_dir/glab-mr-graph
alias_command=$(printf 'exec %s "$@"' "$(shell_quote "$binary_path")")

alias_list=$(glab alias list 2>/dev/null) || error 'cannot inspect glab aliases; refusing to overwrite an unknown alias state'
existing_alias=$(alias_command_from_list "$alias_list")
if [ -n "$existing_alias" ] && [ "$existing_alias" != "$alias_command" ]; then
  error "glab alias mr-graph already exists and is not managed by this installation; remove it manually before installing"
fi

tmp_path=$(mktemp "$install_dir/.glab-mr-graph.download.XXXXXX") || error "cannot create temporary file in $install_dir"
backup_path=
installed=0
cleanup() {
  [ -z "${tmp_path:-}" ] || rm -f "$tmp_path"
  if [ "$installed" -eq 1 ]; then
    rm -f "$binary_path"
    if [ -n "${backup_path:-}" ] && [ -e "$backup_path" ]; then
      mv "$backup_path" "$binary_path" 2>/dev/null || true
    fi
  fi
}
trap cleanup EXIT HUP INT TERM

if [ -n "${GLAB_MR_GRAPH_ASSET_PATH:-}" ]; then
  cp "$GLAB_MR_GRAPH_ASSET_PATH" "$tmp_path" || error "cannot copy local asset: $GLAB_MR_GRAPH_ASSET_PATH"
elif has_command curl; then
  curl -fL --retry 2 --connect-timeout 15 --max-time 120 -o "$tmp_path" "$download_url" || error "download failed: $download_url"
elif has_command wget; then
  wget -q --tries=3 --timeout=15 -O "$tmp_path" "$download_url" || error "download failed: $download_url"
else
  error 'curl or wget is required to download a release asset'
fi

[ -s "$tmp_path" ] || error 'downloaded release asset is empty'
chmod 755 "$tmp_path" || error "cannot make downloaded binary executable: $tmp_path"

if [ -e "$binary_path" ] || [ -L "$binary_path" ]; then
  backup_path=$(mktemp "$install_dir/.glab-mr-graph.backup.XXXXXX") || error "cannot prepare an upgrade backup in $install_dir"
  rm -f "$backup_path"
  mv "$binary_path" "$backup_path" || error "cannot preserve existing binary at $binary_path"
fi
mv "$tmp_path" "$binary_path" || error "cannot install binary at $binary_path"
tmp_path=
installed=1

if ! glab alias set --shell mr-graph "$alias_command"; then
  error "binary installed at $binary_path, but glab alias mr-graph could not be registered"
fi

if [ -n "$backup_path" ]; then
  rm -f "$backup_path"
  backup_path=
fi
installed=0
trap - EXIT HUP INT TERM

printf 'Installed %s\n' "$binary_path"
printf 'Run: glab mr-graph\n'
