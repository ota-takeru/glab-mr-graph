#!/bin/sh
set -eu

warning() {
  printf '%s\n' "glab-mr-graph uninstaller: warning: $*" >&2
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

if [ -n "${GLAB_MR_GRAPH_INSTALL_DIR:-}" ]; then
  requested_install_dir=$GLAB_MR_GRAPH_INSTALL_DIR
elif [ -n "${XDG_BIN_HOME:-}" ]; then
  requested_install_dir=$XDG_BIN_HOME
elif [ -n "${HOME:-}" ]; then
  requested_install_dir=$HOME/.local/bin
else
  warning 'set GLAB_MR_GRAPH_INSTALL_DIR when HOME is not available'
  requested_install_dir=
fi

if [ -n "$requested_install_dir" ] && [ -d "$requested_install_dir" ]; then
  install_dir=$(CDPATH= cd -- "$requested_install_dir" && pwd -P)
else
  install_dir=$requested_install_dir
fi

if [ -n "$install_dir" ]; then
  binary_path=$install_dir/glab-mr-graph
  expected_alias=$(printf 'exec %s "$@"' "$(shell_quote "$binary_path")")
else
  binary_path=
  expected_alias=
fi

if command -v glab >/dev/null 2>&1; then
  if alias_list=$(glab alias list 2>/dev/null); then
    existing_alias=$(alias_command_from_list "$alias_list")
    if [ -n "$existing_alias" ]; then
      if [ -n "$expected_alias" ] && [ "$existing_alias" = "$expected_alias" ]; then
        if glab alias delete mr-graph >/dev/null 2>&1; then
          printf 'Removed glab alias mr-graph\n'
        else
          warning 'could not remove the managed glab alias mr-graph'
        fi
      else
        warning 'preserving existing glab alias mr-graph because it is not managed by this installation'
      fi
    fi
  else
    warning 'could not inspect glab aliases; preserving alias state'
  fi
else
  warning 'glab was not found; preserving alias state'
fi

if [ -n "$binary_path" ] && { [ -e "$binary_path" ] || [ -L "$binary_path" ]; }; then
  if rm -f "$binary_path"; then
    printf 'Removed %s\n' "$binary_path"
  else
    warning "could not remove $binary_path"
  fi
fi
