#!/bin/sh
# Builds macos/build/ng.app. Assets.car and ng.icns are compiled from ng.icon (Icon Composer) with
#   xcrun actool ng.icon --compile . --app-icon ng --platform macosx --minimum-deployment-target 11.0 --output-partial-info-plist /dev/null
set -eu
cd "$(dirname "$0")"
c=build/ng.app/Contents
rm -rf build/ng.app && mkdir -p "$c/MacOS" "$c/Resources"
go build -o "$c/MacOS/ng" ../cmd/ng
cp Info.plist "$c/" && cp ng-launcher "$c/MacOS/" && cp Assets.car ng.icns "$c/Resources/"
codesign --force --sign - build/ng.app
