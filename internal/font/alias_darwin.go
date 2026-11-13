package font

// Neovim's default guifont on macOS starts with "SF Mono", which is only
// registered under its hidden system name.
var familyAliases = map[string]string{"SF Mono": ".SF NS Mono"}
