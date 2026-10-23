#!/bin/bash
# BigBadRescue.app launcher: starts the right build for this Mac (Apple
# silicon or Intel). The app opens its own window and quits when it closes.
BIN="$(cd "$(dirname "$0")/../Resources/bin" && pwd)"
ARCH="$(uname -m)"
[ "$ARCH" = "arm64" ] || ARCH="x86_64"
LOG="$HOME/Library/Logs/BigBadRescue.log"
mkdir -p "$(dirname "$LOG")"
exec "$BIN/bigbadrescue-$ARCH" >>"$LOG" 2>&1
