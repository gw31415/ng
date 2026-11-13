# ng for macOS

> [!WARNING]
> This macOS port is unofficial. It lives only in this fork and is not
> supported by [Shougo/ng](https://github.com/Shougo/ng), which targets Linux.
> Please do not report macOS problems upstream.

This fork builds ng as a macOS app, `ng.app`. Everything macOS-specific lives
in this directory, `.github/workflows/rebase.yml`, `release-macos.yml` and
`.github/chainguard/`.

## Install with Homebrew

```sh
brew install --cask gw31415/tap/ng
brew upgrade --cask --greedy-latest ng   # to update
```

This installs the rolling `nightly` release (no version numbers), signed with
a Developer ID and notarized by Apple, and the Homebrew formulae it needs.
`release-macos.yml` rebuilds it whenever the `rebase` workflow updates `main`;
signing and notarization run in
[gw31415/homebrew-tap](https://github.com/gw31415/homebrew-tap).

## Requirements

- macOS on Apple Silicon (the app is built for arm64)
- [Homebrew](https://brew.sh) with these formulae:

  ```sh
  brew install sdl3 freetype fontconfig neovim   # needed to run ng.app
  brew install go pkgconf                        # needed only to build it
  ```

  `ng.app` links against Homebrew's SDL3, FreeType and fontconfig instead of
  bundling them, so it only runs on a Mac where those are installed.
- Neovim: the latest stable release series or nightly (see the main
  [README](../README.md#requirements)). The app starts `nvim` from a zsh
  login shell, so `nvim` must be on the `PATH` that `zsh -l` sets up
  (for Homebrew, `eval "$(/opt/homebrew/bin/brew shellenv)"` in `~/.zprofile`).

## Build and install

```sh
macos/build-app.sh                          # builds macos/build/ng.app
ditto macos/build/ng.app ~/Applications/ng.app
```

`build-app.sh` builds `cmd/ng`, assembles the bundle and signs it ad hoc
(`codesign --sign -`). The icon files `Assets.car` and `ng.icns` are compiled
from `ng.icon`; the command is at the top of `build-app.sh`.

## Download without Homebrew

Download `ng-macos-arm64.zip` from the
[`nightly` release](https://github.com/gw31415/ng/releases/tag/nightly) and
unzip it. It is the same signed and notarized `ng.app` as the Cask, rebuilt
by `release-macos.yml` whenever the `rebase` workflow (every three hours)
moves `main` onto [Shougo/ng](https://github.com/Shougo/ng).

The Homebrew libraries above must still be installed.
