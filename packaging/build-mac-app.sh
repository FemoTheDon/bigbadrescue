#!/bin/bash
# Builds BigBadRescue.app (Apple silicon + Intel) with ffmpeg bundled.
# Works on macOS or Linux; needs Go 1.25+ and curl.
#
#   ./packaging/build-mac-app.sh            -> dist/BigBadRescue.app
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
APP="$ROOT/dist/BigBadRescue.app"
CACHE="$ROOT/dist/ffmpeg-cache"
FFREL="https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0"

mkdir -p "$CACHE"
for a in arm64 x64; do
  if [ ! -x "$CACHE/ffmpeg-darwin-$a" ]; then
    echo "Downloading ffmpeg for macOS $a…"
    curl -fL "$FFREL/ffmpeg-darwin-$a.gz" | gunzip > "$CACHE/ffmpeg-darwin-$a"
    chmod +x "$CACHE/ffmpeg-darwin-$a"
  fi
done

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"
cp packaging/Info.plist "$APP/Contents/"
cp packaging/launcher.sh "$APP/Contents/MacOS/BigBadRescue"
cp packaging/AppIcon.icns "$APP/Contents/Resources/"
cp LICENSE THIRD-PARTY-NOTICES.md "$APP/Contents/Resources/"
chmod +x "$APP/Contents/MacOS/BigBadRescue"

echo "Building…"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$APP/Contents/Resources/bin/bigbadrescue-arm64" .
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$APP/Contents/Resources/bin/bigbadrescue-x86_64" .
cp "$CACHE/ffmpeg-darwin-arm64" "$APP/Contents/Resources/bin/ffmpeg-arm64"
cp "$CACHE/ffmpeg-darwin-x64" "$APP/Contents/Resources/bin/ffmpeg-x86_64"

# On a Mac, clear the download flag so the app opens without a warning.
if command -v xattr >/dev/null; then xattr -cr "$APP" || true; fi
echo "Done: $APP"
