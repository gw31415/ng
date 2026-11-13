# ng for macOS

> [!WARNING]
> This macOS port is unofficial. It lives only in this fork and is not
> supported by [Shougo/ng](https://github.com/Shougo/ng), which targets Linux.
> Please do not report macOS problems upstream.

This fork builds ng as a macOS app, `ng.app`. Everything macOS-specific lives
in this directory, `.github/workflows/rebase.yml`, `release-macos.yml` and
`.github/chainguard/`.

## Install with Homebrew

Requirements:

- macOS on Apple Silicon
- [Homebrew](https://brew.sh), with `brew shellenv` in `~/.zprofile` (the
  Homebrew installer suggests this). ng.app always starts `nvim` through
  macOS's `/bin/zsh -l`, whatever your login shell is (fish too), so `nvim`
  must be on the `PATH` that `~/.zprofile` or `~/.zshenv` sets up.

```sh
brew install --cask gw31415/tap/ng
brew upgrade --cask --greedy-latest ng   # to update
```

This installs the rolling `nightly` release (no version numbers), signed with
a Developer ID and notarized by Apple, together with the formulae it needs:
`neovim`, `sdl3`, `freetype` and `fontconfig`. `release-macos.yml` rebuilds it
whenever the `rebase` workflow (every three hours) moves `main` onto
[Shougo/ng](https://github.com/Shougo/ng); signing and notarization run in
[gw31415/homebrew-tap](https://github.com/gw31415/homebrew-tap).

## Download the prebuilt app

Requirements:

- macOS on Apple Silicon
- SDL3, FreeType and fontconfig from [Homebrew](https://brew.sh). ng.app
  links against Homebrew's copies in `/opt/homebrew` instead of bundling
  them:

  ```sh
  brew install sdl3 freetype fontconfig
  ```

- Neovim: the latest stable release series or nightly (see the main
  [README](../README.md#requirements)), on the `PATH` that `~/.zprofile` or
  `~/.zshenv` sets up: ng.app always starts `nvim` through `/bin/zsh -l`,
  whatever your login shell is.

Download `ng-macos-arm64.zip` from the
[`nightly` release](https://github.com/gw31415/ng/releases/tag/nightly),
unzip it and move `ng.app` to `/Applications` or `~/Applications`. It is the
same signed and notarized `ng.app` as the Cask; to update, download it again.

## Build from source

Requirements:

- macOS (only Apple Silicon is tested)
- Xcode Command Line Tools (`xcode-select --install`) for `clang`
- [Homebrew](https://brew.sh) with these formulae:

  ```sh
  brew install go pkgconf sdl3 freetype fontconfig
  ```

- Neovim: the latest stable release series or nightly, on the `PATH` that
  `~/.zprofile` or `~/.zshenv` sets up, whatever your login shell is.

```sh
macos/build-app.sh                          # builds macos/build/ng.app
ditto macos/build/ng.app ~/Applications/ng.app
```

`build-app.sh` builds `cmd/ng`, assembles the bundle and signs it ad hoc
(`codesign --sign -`). The icon files `Assets.car` and `ng.icns` are compiled
from `ng.icon`; the command is at the top of `build-app.sh`.
