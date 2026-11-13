#!/bin/sh
# Build ng.app (fork-only, macOS). Usage: macos/build-app.sh [OUTDIR]
# The app links the Homebrew SDL3/FreeType/fontconfig of this machine and
# starts the nvim found on the login shell's PATH.
set -eu
cd "$(dirname "$0")/.."
out=${1:-macos/build}
app="$out/ng.app"

go build -o ng ./cmd/ng
version=$(./ng --version | awk '{print $2}' | sed 's/^v//')
short=${version%%-*}

mkdir -p "$out"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
sed -e "s/@VERSION@/$version/g" -e "s/@SHORT@/$short/g" macos/Info.plist > "$app/Contents/Info.plist"
cp macos/ng-launcher "$app/Contents/MacOS/ng-launcher"
mv ng "$app/Contents/MacOS/ng"

iconset=$(mktemp -d)/ng.iconset
mkdir -p "$iconset"
icons=assets/icons/hicolor
for s in 16 32 128 256 512; do
	cp "$icons/${s}x${s}/apps/ng.png" "$iconset/icon_${s}x${s}.png"
done
cp "$icons/32x32/apps/ng.png" "$iconset/icon_16x16@2x.png"
cp "$icons/64x64/apps/ng.png" "$iconset/icon_32x32@2x.png"
cp "$icons/256x256/apps/ng.png" "$iconset/icon_128x128@2x.png"
cp "$icons/512x512/apps/ng.png" "$iconset/icon_256x256@2x.png"
iconutil -c icns -o "$app/Contents/Resources/ng.icns" "$iconset"
rm -rf "$(dirname "$iconset")"

# Ad-hoc signature, required to run on Apple Silicon.
codesign --force --deep --sign - "$app"
echo "built $app ($version)"
