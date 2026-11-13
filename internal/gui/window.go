package gui

/*
#include "sdl.h"
*/
import "C"

import (
	"fmt"
	"math"
	"runtime"
	"time"
)

// waitingMessage is drawn while Neovim is blocked at a prompt and has not
// accepted the UI yet. Neovim's own screen replaces it after attaching.
var waitingMessage = []string{
	"ng: Neovim is waiting for input and cannot show its screen yet.",
	"Type the answer to its prompt (for example <Enter> or <Esc>).",
}

func (a *App) showWaiting() {
	if a.attached {
		return
	}
	events := []any{[]any{"grid_clear", []any{int64(1)}}}
	for i, line := range waitingMessage {
		cells := []any{}
		for _, r := range line {
			cells = append(cells, []any{string(r)})
		}
		events = append(events, []any{"grid_line", []any{int64(1), int64(i), int64(0), cells, false}})
	}
	a.scr.HandleRedraw(events)
	a.show()
	a.paintDirty()
	a.present()
}

func (a *App) dpi() float64 {
	s := float64(C.SDL_GetWindowDisplayScale(a.win))
	if s <= 0 {
		s = 1
	}
	if runtime.GOOS == "darwin" {
		return 72 * s // macOS points
	}
	return 96 * s
}

// resizeWindowToGrid asks the window manager for a window that fits
// cols x rows, then fits the grid to whatever size the window really got
// (tiling window managers such as i3 decide the size themselves).
func (a *App) resizeWindowToGrid(cols, rows int) {
	if a.shown && a.wmSized {
		// The window manager decides the size (tiling): asking would only
		// shrink the drawing surface inside the tile (shown as black on
		// Wayland). Fit the grid to the window instead, as GVim ends up
		// doing.
		a.updatePixelSize(false)
		return
	}
	a.reqW, a.reqH = cols*a.fonts.CellW, rows*a.fonts.CellH
	d := float64(C.ng_pixel_density(a.win))
	w := int(math.Ceil(float64(cols*a.fonts.CellW) / d))
	h := int(math.Ceil(float64(rows*a.fonts.CellH) / d))
	// No SDL_SyncWindow: a tiling window manager may never apply the size,
	// and waiting for it would stall. The new size arrives as
	// SDL_EVENT_WINDOW_PIXEL_SIZE_CHANGED. Until then the grid is left
	// alone: reading the window size now could give the old size and make
	// ng shrink the grid back. If no event comes (the window manager kept
	// the size), the grid is fitted to the real window after settleDelay.
	C.SDL_SetWindowSize(a.win, C.int(w), C.int(h))
	if a.shown {
		a.settleAt = time.Now().Add(settleDelay)
	}
}

// keepGridFitted puts the grid back to the window's size when the window
// manager decides the window size and the grid changed (":set columns"
// in a tiled window). The change arrives after Neovim's OptionSet
// notification, so it is checked at every flush rather than when the
// notification comes.
func (a *App) keepGridFitted() {
	if !a.shown || !a.wmSized || !a.attached {
		return
	}
	if a.scr.Width == a.gridCols && a.scr.Height == a.gridRows {
		a.fitSentAt = time.Time{}
		return
	}
	// Do not repeat the request while Neovim is still applying it.
	if !a.fitSentAt.IsZero() && time.Since(a.fitSentAt) < 200*time.Millisecond {
		return
	}
	a.fitSentAt = time.Now()
	a.nv.TryResize(a.gridCols, a.gridRows)
}

// settleDelay is how long the window size must stay unchanged after a
// size request before ng takes it as the window manager's answer.
const settleDelay = 300 * time.Millisecond

// settle runs settleDelay after a size request, or after the last size
// change that followed it. The window's size now is the window manager's
// answer: if it is not what ng asked for, the window manager decides
// window sizes (tiling). Single events cannot tell: on sway a tiled
// window first gets the requested size and then the tile, and resizing it
// later only shrinks the drawing surface, reported as the requested size.
func (a *App) settle() {
	a.settleAt = time.Time{}
	a.updatePixelSize(true)
	a.mark(fmt.Sprintf("window size settled at %dx%d px", a.pixW, a.pixH))
	a.paintDirty()
}

// show makes the window visible the first time: with the fonts from the
// config, sized for the grid Neovim uses.
func (a *App) show() {
	if a.shown {
		return
	}
	if a.fonts == nil {
		if err := a.loadOptionFonts(); err != nil {
			a.errorMessage(err.Error())
			if a.loadFonts("", "", 0) != nil {
				return
			}
		}
	}
	a.resizeWindowToGrid(a.scr.Width, a.scr.Height)
	C.SDL_ShowWindow(a.win)
	a.shown = true
	// The window manager answers the size request with one or more size
	// changes (on sway: first the requested size, then the tile). The
	// answer is the size once they have stopped; see settle.
	a.updatePixelSize(false)
	// Resolve the size request within settleDelay even if no event comes
	// (the window got exactly that size), so that a later resize by the
	// user is not mistaken for the answer to it.
	a.settleAt = time.Now().Add(settleDelay)
	a.mark("window shown")
	if a.onShown != nil {
		a.onShown()
	}
}

// updatePixelSize reads the drawable size and resizes the grid to fit.
// answer is true when the size is the window manager's answer to a resize
// request (an event, or the settle timeout): if it differs from what ng
// asked for, the window manager decides window sizes from now on.
func (a *App) updatePixelSize(answer bool) {
	var w, h C.int
	C.SDL_GetWindowSizeInPixels(a.win, &w, &h)
	a.pixW, a.pixH = int(w), int(h)
	if a.fonts == nil || !a.shown {
		return
	}
	if answer && a.reqW > 0 {
		if !a.wmSized && overridden(a.reqW, a.reqH, a.pixW, a.pixH, a.fonts.CellW, a.fonts.CellH) {
			a.wmSized = true
			a.mark("window manager decides the window size")
		}
		a.reqW, a.reqH = 0, 0
	}
	a.gridCols = max(1, a.pixW/a.fonts.CellW)
	a.gridRows = max(1, a.pixH/a.fonts.CellH)
	if a.attached && (a.gridCols != a.scr.Width || a.gridRows != a.scr.Height) {
		a.nv.TryResize(a.gridCols, a.gridRows)
	}
	a.scr.MarkAllDirty()
}
