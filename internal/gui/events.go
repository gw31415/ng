package gui

/*
#include "sdl.h"
*/
import "C"

import (
	"fmt"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/Shougo/ng/internal/input"
	"github.com/Shougo/ng/internal/paint"
)

// handle processes one SDL event and reports whether the window needs to
// be presented again.
func (a *App) handle(e event) bool {
	if e.typ == C.SDL_EVENT_DROP_FILE && !a.attached {
		a.pendingDrops = append(a.pendingDrops, e)
		return false
	}
	if a.nv == nil {
		return false // not connected yet; the window is still hidden
	}
	switch e.typ {
	case C.SDL_EVENT_QUIT, C.SDL_EVENT_WINDOW_CLOSE_REQUESTED:
		// Let Neovim decide: it asks about unsaved buffers, and ng exits
		// when the connection closes.
		a.nv.Command("confirm qall")
	case C.SDL_EVENT_WINDOW_PIXEL_SIZE_CHANGED:
		a.updatePixelSize(false)
		if a.trace != nil {
			a.tracef(time.Now(), "window  %dx%d px", a.pixW, a.pixH)
		}
		if a.reqW > 0 {
			// More size changes may follow: answer once they stop.
			a.settleAt = time.Now().Add(settleDelay)
		}
		a.paintDirty()
		return true
	case C.SDL_EVENT_WINDOW_DISPLAY_SCALE_CHANGED:
		if err := a.loadOptionFonts(); err == nil {
			a.updatePixelSize(false)
			a.paintDirty()
		}
		return true
	case C.SDL_EVENT_WINDOW_EXPOSED:
		return true
	case C.SDL_EVENT_RENDER_TARGETS_RESET, C.SDL_EVENT_RENDER_DEVICE_RESET:
		if a.screenTex != nil {
			C.SDL_DestroyTexture(a.screenTex)
			a.screenTex = nil
		}
		a.resetAtlas()
		a.paintDirty()
		return true
	case C.SDL_EVENT_WINDOW_FOCUS_GAINED, C.SDL_EVENT_WINDOW_FOCUS_LOST:
		a.focused = e.typ == C.SDL_EVENT_WINDOW_FOCUS_GAINED
		if !a.focused {
			a.preedit = paint.Preedit{} // the input method resets on focus loss
		}
		if a.attached {
			a.nv.SetFocus(a.focused)
		}
		return true
	case C.SDL_EVENT_KEY_DOWN:
		if e.repeat && bool(C.ng_key_up_queued(C.int(e.scan))) {
			// The key was already released. With Fcitx 5 under Wayland,
			// SDL sends, on release, the repeats it thinks are due since
			// the press, although it sent them while the key was held:
			// the cursor jumped by as many keys when a key held on the
			// command line was released. No repeat comes after a release.
			if a.trace != nil {
				a.tracef(time.Now(), "key     0x%x repeat after release, dropped", e.key)
			}
			return false
		}
		if a.trace != nil {
			now := time.Now()
			// A repeated key's SDL timestamp can be that of the first
			// press: measure repeats from when ng handled them.
			event := now.Add(-e.age)
			if e.repeat {
				event = now
			}
			a.latency.Key(event, now, fmt.Sprintf("%q", rune(e.typed)))
			a.tracef(now, "key     0x%x typed=%q mod=0x%x repeat=%v kbd=%d stamp=%.3f text-input=%v",
				e.key, rune(e.typed), e.mod, e.repeat, e.kbd, e.stamp, a.textInput)
		}
		a.keyDown(e)
	case C.SDL_EVENT_KEY_UP:
		if a.trace != nil {
			a.tracef(time.Now(), "keyup   0x%x kbd=%d stamp=%.3f", e.key, e.kbd, e.stamp)
		}
	case C.SDL_EVENT_TEXT_EDITING:
		// The input method's preedit text: drawn by ng at the cursor, not
		// sent to Neovim until committed (SDL_EVENT_TEXT_INPUT).
		cursor := e.editStart
		if cursor < 0 {
			cursor = utf8.RuneCountInString(e.text)
		}
		a.preedit = paint.Preedit{Text: e.text, Cursor: cursor, SelLen: e.editLength}
		a.suppressText = false // macOS sends no key event for keys the IME takes
		if a.trace != nil {
			a.tracef(time.Now(), "preedit %q cursor=%d sel=%d", e.text, e.editStart, e.editLength)
		}
		return true
	case C.SDL_EVENT_TEXT_INPUT:
		wasComposing := a.preedit.Text != ""
		a.preedit = paint.Preedit{}
		if a.suppressText && !wasComposing {
			a.suppressText = false
			return false
		}
		a.suppressText = false
		if a.trace != nil {
			now := time.Now()
			a.latency.Key(now.Add(-e.age), now, fmt.Sprintf("%q", e.text))
			a.tracef(now, "commit  %q", e.text)
		}
		a.nv.Input(input.Text(e.text))
		return wasComposing
	case C.SDL_EVENT_MOUSE_BUTTON_DOWN, C.SDL_EVENT_MOUSE_BUTTON_UP,
		C.SDL_EVENT_MOUSE_MOTION, C.SDL_EVENT_MOUSE_WHEEL:
		a.mouse(e)
	case C.SDL_EVENT_DROP_FILE:
		if e.drop != "" {
			a.nv.Notify("nvim_exec_lua", "vim.cmd.drop(vim.fn.fnameescape(...))", []any{e.drop})
		}
	}
	return false
}

func mods(m uint16) input.Mod {
	var r input.Mod
	if m&C.SDL_KMOD_SHIFT != 0 {
		r |= input.ModShift
	}
	if m&C.SDL_KMOD_CTRL != 0 {
		r |= input.ModCtrl
	}
	if m&C.SDL_KMOD_ALT != 0 {
		r |= input.ModAlt
	}
	if m&C.SDL_KMOD_GUI != 0 {
		r |= input.ModSuper
	}
	return r
}

func specialKey(k uint32) input.Key {
	switch k {
	case C.SDLK_ESCAPE:
		return input.KeyEscape
	case C.SDLK_RETURN, C.SDLK_KP_ENTER:
		return input.KeyEnter
	case C.SDLK_TAB:
		return input.KeyTab
	case C.SDLK_BACKSPACE:
		return input.KeyBackspace
	case C.SDLK_DELETE:
		return input.KeyDelete
	case C.SDLK_INSERT:
		return input.KeyInsert
	case C.SDLK_HOME:
		return input.KeyHome
	case C.SDLK_END:
		return input.KeyEnd
	case C.SDLK_PAGEUP:
		return input.KeyPageUp
	case C.SDLK_PAGEDOWN:
		return input.KeyPageDown
	case C.SDLK_UP:
		return input.KeyUp
	case C.SDLK_DOWN:
		return input.KeyDown
	case C.SDLK_LEFT:
		return input.KeyLeft
	case C.SDLK_RIGHT:
		return input.KeyRight
	case C.SDLK_SPACE:
		return input.KeySpace
	}
	if k >= C.SDLK_F1 && k <= C.SDLK_F12 {
		return input.KeyF1 + input.Key(k-C.SDLK_F1)
	}
	return input.KeyNone
}

func (a *App) keyDown(e event) {
	if a.preedit.Text != "" {
		// The input method is composing: Enter, Escape, arrows and so on
		// belong to it, not to Neovim.
		return
	}
	a.suppressText = false
	m := mods(e.mod)
	if k := specialKey(e.key); k != input.KeyNone {
		if s := input.SpecialKey(k, m); s != "" {
			a.nv.Input(s)
			a.suppressText = m&(input.ModCtrl|input.ModAlt|input.ModSuper) != 0
		} else if k == input.KeySpace && !a.textInput {
			a.nv.Input(" ") // no text input events while it is off
		}
		if k == input.KeyEscape {
			a.leavingTextMode()
		}
		return
	}
	if e.key == '[' && m&input.ModCtrl != 0 {
		defer a.leavingTextMode()
	}
	// Ctrl+Shift+V pastes the clipboard, as in terminal emulators.
	if e.key < C.SDLK_SCANCODE_MASK && input.IsPaste(rune(e.key), m) {
		a.paste()
		a.suppressText = true
		return
	}
	// Printable key with Ctrl/Alt/Super: SDL may or may not also send a
	// text event for it, so the next text event is dropped.
	if e.key < C.SDLK_SCANCODE_MASK {
		if s := input.ChordKey(rune(e.key), m); s != "" {
			a.nv.Input(s)
			a.suppressText = true
			return
		}
	}
	// With text input off (Normal mode, see updateTextInput) SDL sends no
	// text events: the key's own character is the text.
	if !a.textInput {
		a.nv.Input(input.DirectText(rune(e.typed), m))
	}
}

// paste sends the clipboard to Neovim with nvim_paste.
func (a *App) paste() {
	p := C.ng_clipboard_text()
	if p == nil {
		return
	}
	text := C.GoString(p)
	C.SDL_free(unsafe.Pointer(p))
	// Paste waits for Neovim to store the text; never block the window.
	go a.nv.Paste(text)
}

func (a *App) cellAt(x, y float32) (row, col int) {
	d := float32(C.ng_pixel_density(a.win))
	col = int(x*d) / a.fonts.CellW
	row = int(y*d) / a.fonts.CellH
	return min(max(row, 0), a.scr.Height-1), min(max(col, 0), a.scr.Width-1)
}

func (a *App) mouse(e event) {
	if !a.scr.MouseEnabled {
		return
	}
	mod := input.MouseModifier(mods(uint16(C.SDL_GetModState())))
	switch e.typ {
	case C.SDL_EVENT_MOUSE_BUTTON_DOWN, C.SDL_EVENT_MOUSE_BUTTON_UP:
		var name string
		switch e.button {
		case C.SDL_BUTTON_LEFT:
			name = "left"
		case C.SDL_BUTTON_MIDDLE:
			name = "middle"
		case C.SDL_BUTTON_RIGHT:
			name = "right"
		default:
			return
		}
		row, col := a.cellAt(e.x, e.y)
		a.mouseRow, a.mouseCol = row, col
		if e.typ == C.SDL_EVENT_MOUSE_BUTTON_DOWN {
			a.mouseButton = name
			a.nv.InputMouse(name, "press", mod, row, col)
		} else {
			a.mouseButton = ""
			a.nv.InputMouse(name, "release", mod, row, col)
		}
	case C.SDL_EVENT_MOUSE_MOTION:
		row, col := a.cellAt(e.x, e.y)
		moved := row != a.mouseRow || col != a.mouseCol
		a.mouseRow, a.mouseCol = row, col
		if a.mouseButton != "" && moved {
			a.nv.InputMouse(a.mouseButton, "drag", mod, row, col)
		}
	case C.SDL_EVENT_MOUSE_WHEEL:
		row, col := a.mouseRow, a.mouseCol
		switch {
		case e.y > 0:
			a.nv.InputMouse("wheel", "up", mod, row, col)
		case e.y < 0:
			a.nv.InputMouse("wheel", "down", mod, row, col)
		}
		switch {
		case e.x > 0:
			a.nv.InputMouse("wheel", "right", mod, row, col)
		case e.x < 0:
			a.nv.InputMouse("wheel", "left", mod, row, col)
		}
	}
}
