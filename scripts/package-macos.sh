#!/bin/sh
set -eu

if [ "$(uname -s)" != Darwin ]; then
    printf '%s\n' 'Packaging requires macOS with the Xcode Command Line Tools.' >&2
    exit 1
fi

root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
cd "$root"

name=telegram-messages-sender
minimum=15.0
output="$root/dist"
mkdir -p "$output"
stage=$(mktemp -d "$output/.macos-arm64.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM

bundle="$stage/$name.app"
mkdir -p "$bundle/Contents/MacOS"
plist="$bundle/Contents/Info.plist"
cp packaging/macos/Info.plist "$plist"
plutil -lint "$plist"
test "$(plutil -extract CFBundleExecutable raw "$plist")" = "$name"
test "$(plutil -extract CFBundleIdentifier raw "$plist")" = "$name"
test "$(plutil -extract CFBundlePackageType raw "$plist")" = APPL
test "$(plutil -extract LSMinimumSystemVersion raw "$plist")" = "$minimum"

build() {
    MACOSX_DEPLOYMENT_TARGET="$minimum" CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
        go build -trimpath -buildvcs=false -ldflags='-s -w -linkmode=external' -o "$1" "$2"
}

app="$bundle/Contents/MacOS/$name"
cli="$stage/tmsctl"
build "$app" "./cmd/$name"
build "$cli" ./cmd/tmsctl

verify_binary() {
    if [ "$(xcrun lipo -archs "$1")" != arm64 ]; then
        printf 'Expected an arm64 executable at %s\n' "$1" >&2
        exit 1
    fi
    metadata=$(xcrun vtool -show-build "$1")
    printf '%s\n' "$metadata"
    if ! printf '%s\n' "$metadata" | awk -v minimum="$minimum" '
        $1 == "platform" && $2 == "MACOS" { macos = 1 }
        $1 == "minos" && $2 == minimum { target = 1 }
        END { exit !(macos && target) }
    '; then
        printf 'Expected a macOS %s deployment target at %s\n' "$minimum" "$1" >&2
        exit 1
    fi
}

verify_binary "$app"
verify_binary "$cli"

codesign --force --sign - "$bundle"
codesign --force --sign - "$cli"
codesign --verify --strict "$bundle"
codesign --verify --strict "$cli"

rm -rf "$output/$name.app"
mv "$bundle" "$output/"
mv -f "$cli" "$output/tmsctl"
printf 'Built %s\nBuilt %s\n' "$output/$name.app" "$output/tmsctl"
