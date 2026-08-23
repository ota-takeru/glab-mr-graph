#!/bin/sh
set -eu

fail() {
  printf 'install_test.sh: %s\n' "$*" >&2
  exit 1
}

assert_eq() {
  [ "$1" = "$2" ] || fail "expected <$1> to equal <$2>"
}

assert_file_contains() {
  grep -F "$2" "$1" >/dev/null 2>&1 || fail "$1 does not contain <$2>"
}

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/glab-mr-graph-install.XXXXXX")
trap 'rm -rf "$tmp_root"' EXIT HUP INT TERM

fake_bin=$tmp_root/fake-bin
install_dir=$tmp_root/'install dir'
mkdir -p "$fake_bin" "$install_dir"
export FAKE_GLAB_ALIAS=$tmp_root/alias
export FAKE_GLAB_LOG=$tmp_root/glab.log
export FAKE_DOWNLOAD_LOG=$tmp_root/download.log
export FAKE_DOWNLOAD_ASSET=$tmp_root/download-asset
: >"$FAKE_GLAB_ALIAS"
: >"$FAKE_GLAB_LOG"
: >"$FAKE_DOWNLOAD_LOG"

cat >"$fake_bin/glab" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_GLAB_LOG"
case "${1:-} ${2:-}" in
  'alias list')
    printf 'Alias\tCommand\n'
    if [ -s "$FAKE_GLAB_ALIAS" ]; then
      printf 'mr-graph\t%s\n' "$(cat "$FAKE_GLAB_ALIAS")"
    fi
    ;;
  'alias set')
    [ "${FAKE_GLAB_SET_FAIL:-0}" != 1 ] || exit 9
    printf '%s' "${5:-}" >"$FAKE_GLAB_ALIAS"
    ;;
  'alias delete')
    : >"$FAKE_GLAB_ALIAS"
    ;;
  *)
    if [ "${1:-}" = mr-graph ]; then
      shift
      command=$(cat "$FAKE_GLAB_ALIAS")
      exec sh -c "$command" mr-graph "$@"
    fi
    exit 2
    ;;
esac
EOF

cat >"$fake_bin/curl" <<'EOF'
#!/bin/sh
set -eu
[ "${FAKE_DOWNLOAD_FAIL:-0}" != 1 ] || exit 22
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    http*) url=$1; shift ;;
    *) shift ;;
  esac
done
printf '%s\n' "$url" >>"$FAKE_DOWNLOAD_LOG"
cp "$FAKE_DOWNLOAD_ASSET" "$output"
EOF
chmod 755 "$fake_bin/glab" "$fake_bin/curl"

asset_v1=$tmp_root/asset-v1
asset_v2=$tmp_root/asset-v2
cat >"$asset_v1" <<'EOF'
#!/bin/sh
printf 'fixture-v1:%s\n' "$*"
EOF
cat >"$asset_v2" <<'EOF'
#!/bin/sh
printf 'fixture-v2:%s\n' "$*"
EOF
chmod 755 "$asset_v1" "$asset_v2"
cp "$asset_v2" "$FAKE_DOWNLOAD_ASSET"

export PATH=$fake_bin:$PATH
export GLAB_MR_GRAPH_INSTALL_DIR=$install_dir
export GLAB_MR_GRAPH_ASSET_PATH=$asset_v1

sh "$repo_dir/scripts/install.sh" >/dev/null
binary_path=$install_dir/glab-mr-graph
[ -x "$binary_path" ] || fail 'local fixture was not installed as an executable'
assert_eq "$(glab mr-graph one 'two words')" 'fixture-v1:one two words'
expected_alias="exec '$binary_path' \"\$@\""
assert_eq "$(cat "$FAKE_GLAB_ALIAS")" "$expected_alias"

GLAB_MR_GRAPH_ASSET_PATH=$asset_v2 sh "$repo_dir/scripts/install.sh" >/dev/null
assert_eq "$(glab mr-graph upgraded)" 'fixture-v2:upgraded'

export FAKE_GLAB_SET_FAIL=1
if GLAB_MR_GRAPH_ASSET_PATH=$asset_v1 sh "$repo_dir/scripts/install.sh" >/dev/null 2>&1; then
  fail 'alias registration failure unexpectedly succeeded'
fi
unset FAKE_GLAB_SET_FAIL
assert_eq "$(glab mr-graph rolled-back)" 'fixture-v2:rolled-back'

unset GLAB_MR_GRAPH_ASSET_PATH
export FAKE_DOWNLOAD_FAIL=1
if sh "$repo_dir/scripts/install.sh" >/dev/null 2>&1; then
  fail 'download failure unexpectedly succeeded'
fi
unset FAKE_DOWNLOAD_FAIL
assert_eq "$(glab mr-graph preserved)" 'fixture-v2:preserved'

sh "$repo_dir/scripts/install.sh" >/dev/null
assert_file_contains "$FAKE_DOWNLOAD_LOG" '/releases/latest/download/linux-amd64'
GLAB_MR_GRAPH_VERSION=v1.2.3 sh "$repo_dir/scripts/install.sh" >/dev/null
assert_file_contains "$FAKE_DOWNLOAD_LOG" '/releases/download/v1.2.3/linux-amd64'

if GLAB_MR_GRAPH_VERSION=v1.bad.3 sh "$repo_dir/scripts/install.sh" >/dev/null 2>&1; then
  fail 'invalid pinned version unexpectedly succeeded'
fi

printf '%s' 'exec something-else "$@"' >"$FAKE_GLAB_ALIAS"
cp "$asset_v1" "$binary_path"
if GLAB_MR_GRAPH_ASSET_PATH=$asset_v2 sh "$repo_dir/scripts/install.sh" >/dev/null 2>&1; then
  fail 'unrelated alias collision unexpectedly succeeded'
fi
assert_eq "$("$binary_path" collision)" 'fixture-v1:collision'
sh "$repo_dir/scripts/uninstall.sh" >/dev/null 2>&1
assert_eq "$(cat "$FAKE_GLAB_ALIAS")" 'exec something-else "$@"'
[ ! -e "$binary_path" ] || fail 'uninstaller did not remove the exact configured binary'

: >"$FAKE_GLAB_ALIAS"
GLAB_MR_GRAPH_ASSET_PATH=$asset_v2 sh "$repo_dir/scripts/install.sh" >/dev/null
sh "$repo_dir/scripts/uninstall.sh" >/dev/null
[ ! -s "$FAKE_GLAB_ALIAS" ] || fail 'managed alias was not removed'
[ ! -e "$binary_path" ] || fail 'managed binary was not removed'

export FAKE_GLAB_SET_FAIL=1
if GLAB_MR_GRAPH_ASSET_PATH=$asset_v2 sh "$repo_dir/scripts/install.sh" >/dev/null 2>&1; then
  fail 'first-install alias failure unexpectedly succeeded'
fi
unset FAKE_GLAB_SET_FAIL
[ ! -e "$binary_path" ] || fail 'first-install alias failure left a binary behind'

printf 'install_test.sh: ok\n'
