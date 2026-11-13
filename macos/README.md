# ng on macOS (fork)

Upstream ng is Linux-only; this fork carries a few small patches so that it
builds and runs on macOS. They are kept minimal so the fork can be rebased
onto upstream.

## Build

```sh
brew install go sdl3 freetype fontconfig pkgconf neovim
go build -o ng ./cmd/ng && ./ng -f
```

## ng.app

```sh
macos/build-app.sh            # creates macos/build/ng.app
cp -R macos/build/ng.app /Applications/
```

The app starts `nvim` from your login shell's `PATH` (Homebrew's
`shellenv` in `~/.zprofile` is enough) in your home directory. Files
opened with it ("Open With", drag onto the Dock icon) are opened in
Neovim. It links the Homebrew libraries of the machine it was built on,
so build it on the Mac that runs it.

## Differences from Linux

- Font sizes are computed at 96 dpi, so `guifont` sizes look about 1.33x
  larger than in other Mac apps; pick a smaller size (e.g. `Menlo:h11`).
- Paste is Ctrl+Shift+V. For Cmd+V, map it in Neovim, for example
  `vim.keymap.set({"i","c"}, "<D-v>", "<C-r>+")`. Cmd+Q/W/H/M are taken by
  the application menu.
