#!/usr/bin/env bash
# Install Chiling from this checkout.
#
# macOS: build the CLI and daemon, wrap them in Chiling.app, and replace
# /Applications/Chiling.app. There is no GUI yet (the mygo.dev shell is not
# in this tree). LSUIElement keeps the bundle out of the Dock until a UI
# replaces Contents/MacOS/Chiling.
#
# Linux and other Unix: install the CLI binaries into a prefix. The .app
# bundle is macOS-only.
#
# This script does not trust the MITM CA and does not change the system proxy.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_NAME="Chiling"
BUNDLE_ID="dev.mygo.chiling"
MIN_GO_MAJOR=1
MIN_GO_MINOR=26

APP_DIR="/Applications"
PREFIX=""
LINK_CLI=1
DRY_RUN=0
UNINSTALL=0
OS="$(uname -s)"

usage() {
	cat <<'EOF'
Usage: ./scripts/install.sh [options]

Build this checkout and install it locally.

  --app-dir DIR     Directory that receives Chiling.app (macOS, default /Applications).
  --prefix DIR      CLI location. A path ending in /bin is used as-is; otherwise
                    DIR/bin is used. On macOS, if --app-dir is omitted and DIR's
                    name is Applications, DIR is also where the .app is placed.
  --no-cli-link     Do not link chiling onto PATH.
  --dry-run         Print the actions and do not change disk or stop processes.
  --uninstall       Remove the app bundle and the CLI links this script installed.
  -h, --help        Show this help.

Examples:
  ./scripts/install.sh
  ./scripts/install.sh --app-dir "$HOME/Applications"
  ./scripts/install.sh --prefix "$HOME/.local"
  ./scripts/install.sh --dry-run
  ./scripts/install.sh --uninstall

After a macOS install the script does not trust a CA or set a proxy. Run:
  chiling daemon start
  chiling mitm install-ca
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

log() {
	echo "$@"
}

expand_path() {
	local p=$1
	case "$p" in
	\~) printf '%s\n' "$HOME" ;;
	\~/*) printf '%s\n' "$HOME/${p#\~/}" ;;
	*) printf '%s\n' "$p" ;;
	esac
}

run() {
	if [[ "$DRY_RUN" -eq 1 ]]; then
		printf 'dry-run:'
		printf ' %q' "$@"
		printf '\n'
		return 0
	fi
	"$@"
}

srun() {
	if [[ -n "${SUDO:-}" ]]; then
		run sudo "$@"
	else
		run "$@"
	fi
}

need_go() {
	if ! command -v go >/dev/null 2>&1; then
		die "go is not installed. Chiling needs Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR} or newer."
	fi
	local line ver major rest minor
	line="$(go version 2>/dev/null || true)"
	ver="$(printf '%s\n' "$line" | awk '{print $3}' | sed 's/^go//')"
	major="${ver%%.*}"
	rest="${ver#*.}"
	minor="${rest%%.*}"
	if [[ -z "$major" || -z "$minor" || "$major" == "$ver" ]]; then
		die "could not parse the Go version from: ${line:-<empty>}"
	fi
	if [[ "$major" -lt "$MIN_GO_MAJOR" ]] || { [[ "$major" -eq "$MIN_GO_MAJOR" ]] && [[ "$minor" -lt "$MIN_GO_MINOR" ]]; }; then
		die "Go ${ver} is too old. Chiling needs Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR} or newer (go.mod asks for 1.26.3)."
	fi
	log "using $(go version)"
}

git_version() {
	local desc commit
	desc="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || true)"
	commit="$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
	if [[ "$desc" =~ ^v?([0-9]+\.[0-9]+\.[0-9]+) ]]; then
		VERSION="${BASH_REMATCH[1]}"
	else
		VERSION="0.1.0"
	fi
	COMMIT="$commit"
	if [[ "$desc" =~ ^[0-9a-f]+$ ]]; then
		BUILD="$commit"
	else
		BUILD="${desc:-$commit}"
	fi
}

build_bins() {
	local out=$1
	local ldflags
	ldflags="-s -w -X github.com/lidessen/chiling/internal/version.Version=${VERSION} -X github.com/lidessen/chiling/internal/version.Commit=${COMMIT}"
	mkdir -p "$out"
	log "building chiling, chilingd, and chiling-plugin-host (${VERSION} ${COMMIT})"
	(
		cd "$ROOT"
		CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out/chiling" ./cmd/chiling
		CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out/chilingd" ./cmd/chilingd
		CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out/chiling-plugin-host" ./cmd/chiling-plugin-host
	)
}

resolve_dirs() {
	if [[ -n "$PREFIX" ]]; then
		PREFIX="$(expand_path "$PREFIX")"
		local base
		base="$(basename "$PREFIX")"
		if [[ "$OS" == "Darwin" && -z "${APP_DIR_SET:-}" && "$base" == "Applications" ]]; then
			APP_DIR="$PREFIX"
			CLI_DIR=""
		elif [[ "$base" == "bin" ]]; then
			CLI_DIR="$PREFIX"
		else
			CLI_DIR="${PREFIX}/bin"
		fi
	fi
	if [[ -z "${CLI_DIR:-}" ]]; then
		if [[ -w /usr/local/bin ]] || [[ "$DRY_RUN" -eq 1 && "$OS" == "Darwin" ]]; then
			CLI_DIR="/usr/local/bin"
		else
			CLI_DIR="${HOME}/.local/bin"
		fi
	fi
	APP_DIR="$(expand_path "$APP_DIR")"
}

warn_path() {
	local dir=$1 entry
	local IFS=':'
	for entry in $PATH; do
		if [[ "$entry" == "$dir" ]]; then
			return 0
		fi
	done
	log "warning: ${dir} is not on PATH. Add it to your shell profile, for example:"
	log "  export PATH=\"${dir}:\$PATH\""
}

stop_running() {
	if [[ "$DRY_RUN" -eq 1 ]]; then
		log "dry-run: would stop a running daemon (chiling daemon stop) and quit Chiling.app"
		return 0
	fi
	local cli=""
	if [[ -n "${CLI_DIR:-}" && -x "${CLI_DIR}/chiling" ]]; then
		cli="${CLI_DIR}/chiling"
	elif command -v chiling >/dev/null 2>&1; then
		cli="$(command -v chiling)"
	fi
	if [[ -n "$cli" ]]; then
		"$cli" daemon stop >/dev/null 2>&1 || true
	fi
	if [[ "$OS" == "Darwin" ]]; then
		osascript -e 'tell application "Chiling" to quit' >/dev/null 2>&1 || true
	fi
}

write_plist() {
	local dest=$1
	cat >"$dest" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>${APP_NAME}</string>
	<key>CFBundleDisplayName</key>
	<string>${APP_NAME}</string>
	<key>CFBundleIdentifier</key>
	<string>${BUNDLE_ID}</string>
	<key>CFBundleVersion</key>
	<string>${BUILD}</string>
	<key>CFBundleShortVersionString</key>
	<string>${VERSION}</string>
	<key>CFBundleExecutable</key>
	<string>${APP_NAME}</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>LSMinimumSystemVersion</key>
	<string>12.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
EOF
}

write_launcher() {
	local dest=$1
	cat >"$dest" <<'EOF'
#!/bin/bash
# Finder launches this with no arguments. A future UI can replace this file.
# The CLI and daemon stay beside it so `chiling daemon start` finds chilingd.
set -euo pipefail
dir="$(cd "$(dirname "$0")" && pwd)"
if [[ $# -eq 0 ]]; then
	exec "$dir/chiling" daemon start
fi
exec "$dir/chiling" "$@"
EOF
	chmod 755 "$dest"
}

assemble_app() {
	local bins=$1 stage=$2
	local app="${stage}/${APP_NAME}.app"
	local macos="${app}/Contents/MacOS"
	local res="${app}/Contents/Resources"
	mkdir -p "$macos" "$res"
	cp "$bins/chiling" "$bins/chilingd" "$bins/chiling-plugin-host" "$macos/"
	chmod 755 "$macos/chiling" "$macos/chilingd" "$macos/chiling-plugin-host"
	write_launcher "$macos/${APP_NAME}"
	write_plist "${app}/Contents/Info.plist"
	cat >"${res}/README.txt" <<EOF
Chiling ${VERSION} (${COMMIT})

This bundle has no GUI yet. The mygo.dev shell is not part of this build.
Contents/MacOS holds:
  Chiling                 launcher (double-click starts the daemon)
  chiling                 CLI
  chilingd                daemon
  chiling-plugin-host     plugin host entry

A later UI can replace Contents/MacOS/Chiling and set LSUIElement to false
in Contents/Info.plist. Re-run scripts/install.sh to replace this app in place.
EOF
}

codesign_and_unquarantine() {
	local app=$1
	if [[ "$DRY_RUN" -eq 1 ]]; then
		log "dry-run: codesign --force --deep --sign - ${app}"
		log "dry-run: xattr -dr com.apple.quarantine ${app}"
		return 0
	fi
	if command -v codesign >/dev/null 2>&1; then
		codesign --force --deep --sign - "$app"
	else
		log "warning: codesign is not available; Gatekeeper may block the app"
	fi
	if command -v xattr >/dev/null 2>&1; then
		xattr -dr com.apple.quarantine "$app" 2>/dev/null || true
	fi
}

link_cli_into() {
	local bin_dir=$1
	local target_dir=$2
	if [[ "$LINK_CLI" -eq 0 ]]; then
		log "skipping CLI link (--no-cli-link)"
		return 0
	fi
	local name
	for name in chiling chilingd chiling-plugin-host; do
		srun ln -sfn "${target_dir}/${name}" "${bin_dir}/${name}"
	done
	warn_path "$bin_dir"
}

replace_app() {
	local new_app=$1
	local dest="${APP_DIR}/${APP_NAME}.app"
	local backup="${APP_DIR}/.${APP_NAME}.app.previous"
	if [[ ! -d "$APP_DIR" ]]; then
		srun mkdir -p "$APP_DIR"
	fi
	if [[ -e "$dest" ]]; then
		srun rm -rf "$backup"
		srun mv "$dest" "$backup"
	fi
	if ! srun mv "$new_app" "$dest"; then
		if [[ -e "$backup" ]]; then
			srun mv "$backup" "$dest" || true
		fi
		die "could not move the new app into ${dest}; the previous bundle was put back if it was still there"
	fi
	srun rm -rf "$backup"
	log "installed ${dest}"
}

install_macos() {
	local work bins app
	if [[ "$DRY_RUN" -eq 1 ]]; then
		log "dry-run: would build into a temp dir and assemble ${APP_NAME}.app"
		log "dry-run: bundle id ${BUNDLE_ID}, version ${VERSION} (${COMMIT}), LSUIElement=true"
		log "dry-run: would replace ${APP_DIR}/${APP_NAME}.app"
		link_cli_into "$CLI_DIR" "${APP_DIR}/${APP_NAME}.app/Contents/MacOS"
		codesign_and_unquarantine "${APP_DIR}/${APP_NAME}.app"
		return 0
	fi
	work="$(mktemp -d "${TMPDIR:-/tmp}/chiling-install.XXXXXX")"
	bins="${work}/bin"
	build_bins "$bins"
	assemble_app "$bins" "$work"
	app="${work}/${APP_NAME}.app"
	replace_app "$app"
	codesign_and_unquarantine "${APP_DIR}/${APP_NAME}.app"
	link_cli_into "$CLI_DIR" "${APP_DIR}/${APP_NAME}.app/Contents/MacOS"
	rm -rf "$work"
}

install_cli_only() {
	local work
	log "this system is ${OS}. Chiling.app is macOS-only; installing the CLI binaries."
	if [[ "$DRY_RUN" -eq 1 ]]; then
		log "dry-run: would build chiling, chilingd, and chiling-plugin-host into ${CLI_DIR}"
		warn_path "$CLI_DIR"
		return 0
	fi
	work="$(mktemp -d "${TMPDIR:-/tmp}/chiling-install.XXXXXX")"
	build_bins "$work"
	srun mkdir -p "$CLI_DIR"
	srun cp "$work/chiling" "$work/chilingd" "$work/chiling-plugin-host" "$CLI_DIR/"
	srun chmod 755 "$CLI_DIR/chiling" "$CLI_DIR/chilingd" "$CLI_DIR/chiling-plugin-host"
	rm -rf "$work"
	warn_path "$CLI_DIR"
	log "installed chiling, chilingd, and chiling-plugin-host in ${CLI_DIR}"
}

uninstall() {
	local dest="${APP_DIR}/${APP_NAME}.app"
	stop_running
	if [[ "$OS" == "Darwin" ]]; then
		if [[ -e "$dest" || "$DRY_RUN" -eq 1 ]]; then
			srun rm -rf "$dest"
			log "removed ${dest}"
		fi
		if [[ "$LINK_CLI" -eq 1 ]]; then
			local name link
			for name in chiling chilingd chiling-plugin-host; do
				link="${CLI_DIR}/${name}"
				if [[ -L "$link" ]]; then
					srun rm -f "$link"
				fi
			done
		fi
	else
		if [[ "$LINK_CLI" -eq 1 ]]; then
			local name
			for name in chiling chilingd chiling-plugin-host; do
				if [[ -e "${CLI_DIR}/${name}" ]]; then
					srun rm -f "${CLI_DIR}/${name}"
				fi
			done
			log "removed CLI binaries from ${CLI_DIR}"
		fi
	fi
}

next_steps() {
	cat <<'EOF'

Chiling is installed. This script did not trust a certificate or change your proxy.

Next:
  chiling daemon start
  chiling mitm install-ca

Point a client at the proxy when you want to capture traffic:
  export HTTP_PROXY=http://localhost:8080 HTTPS_PROXY=http://localhost:8080
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--app-dir)
		[[ $# -ge 2 ]] || die "--app-dir needs a directory"
		APP_DIR="$2"
		APP_DIR_SET=1
		shift 2
		;;
	--prefix)
		[[ $# -ge 2 ]] || die "--prefix needs a directory"
		PREFIX="$2"
		shift 2
		;;
	--no-cli-link)
		LINK_CLI=0
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--uninstall)
		UNINSTALL=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1 (try --help)"
		;;
	esac
done

need_go
git_version
resolve_dirs

if [[ "$OS" == "Darwin" ]]; then
	parent="$(dirname "$APP_DIR")"
	if [[ -d "$APP_DIR" && ! -w "$APP_DIR" ]]; then
		SUDO="sudo"
	elif [[ ! -d "$APP_DIR" && ! -w "$parent" ]]; then
		SUDO="sudo"
	fi
	if [[ -n "${SUDO:-}" ]]; then
		log "note: ${APP_DIR} is not writable; install will use sudo for the copy only"
	fi
elif [[ -n "${CLI_DIR:-}" && -e "$CLI_DIR" && ! -w "$CLI_DIR" ]]; then
	SUDO="sudo"
	log "note: ${CLI_DIR} is not writable; install will use sudo for the copy only"
fi

if [[ "$UNINSTALL" -eq 1 ]]; then
	uninstall
	exit 0
fi

stop_running

if [[ "$OS" == "Darwin" ]]; then
	install_macos
else
	install_cli_only
fi

if [[ "$DRY_RUN" -eq 0 ]]; then
	next_steps
fi
