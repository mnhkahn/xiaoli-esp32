#!/usr/bin/env bash
# build-app.sh — compile xiaoli-mac and wrap it in a .app bundle so
# macOS will surface the microphone permission prompt on first run.
#
# Usage:  ./scripts/build-app.sh
# Output: ./build/xiaoli-mac.app  (and a copy at ./build/xiaoli-mac)
set -euo pipefail

cd "$(dirname "$0")/.."

APP_NAME="小李"
BUNDLE_ID="com.xiaoli.mac"
BIN_NAME="xiaoli-mac"
BUILD_DIR="build"
APP_DIR="${BUILD_DIR}/${APP_NAME}.app"
CONTENTS="${APP_DIR}/Contents"
MACOS_DIR="${CONTENTS}/MacOS"
RES_DIR="${CONTENTS}/Resources"

# 1. Build the binary.
echo "==> go build (CGO)"
CGO_ENABLED=1 go build -o "${BUILD_DIR}/${BIN_NAME}" ./cmd/xiaoli-mac

# 2. Lay out the .app bundle.
echo "==> pack ${APP_DIR}"
rm -rf "${APP_DIR}"
mkdir -p "${MACOS_DIR}" "${RES_DIR}"
cp "${BUILD_DIR}/${BIN_NAME}" "${MACOS_DIR}/${BIN_NAME}"
cp build/Info.plist "${CONTENTS}/Info.plist"

# 3. Ad-hoc codesign so macOS treats this as a real app (lets the
# TCC system attach a stable identity for the permission grant).
echo "==> codesign --force --deep --sign -"
codesign --force --deep --sign - "${APP_DIR}" 2>&1 | sed 's/^/    /'

echo
echo "Built: ${APP_DIR}"
echo
echo "First run will trigger the microphone prompt:"
echo "  open ${APP_DIR}"
echo
echo "If the prompt does not appear, reset the permission database for"
echo "this app once, then re-run:"
echo "  tccutil reset Microphone ${BUNDLE_ID}"
