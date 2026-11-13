// Package gui is ng's window: SDL3 for the window, input and drawing (with
// SDL_Renderer, so it runs on OpenGL, Vulkan or SDL's software renderer
// under xrdp), and the paint package for what to draw.
package gui

/*
#include <stdlib.h>
#include "sdl.h"
*/
import "C"

import (
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/Shougo/ng/internal/atlas"
	"github.com/Shougo/ng/internal/font"
	"github.com/Shougo/ng/internal/frame"
	"github.com/Shougo/ng/internal/input"
	"github.com/Shougo/ng/internal/latency"
	"github.com/Shougo/ng/internal/msgpack"
	"github.com/Shougo/ng/internal/nvim"
	"github.com/Shougo/ng/internal/paint"
	"github.com/Shougo/ng/internal/rpc"
	"github.com/Shougo/ng/internal/screen"
)

const atlasSize = 2048

// bootstrap runs in Neovim before the UI attaches. ng installs nothing on
// the runtimepath; everything it needs is sent here.
const bootstrap = nvim.Bootstrap

// Options configure Run.
type Options struct {
	Cols, Rows int
	Trace      io.Writer // receives a log of redraws and frames, or nil
	Start      time.Time // when the process started (for startup timing)
	OnShown    func()    // called once the window is first shown, or nil
}

// Connector starts or attaches to Neovim. Run calls it on a goroutine, in
// parallel with creating the window.
type Connector func(onNotify rpc.NotificationHandler) (*nvim.Nvim, error)

type rect struct{ x, y, w, h int }

// App is the running GUI. All fields are owned by the main thread except
// the redraw queue.
type App struct {
	nv  *nvim.Nvim
	scr *screen.Screen

	win       *C.SDL_Window
	ren       *C.SDL_Renderer
	screenTex *C.SDL_Texture
	texW      int
	texH      int
	atlasTex  *C.SDL_Texture
	packer    *atlas.Packer
	glyphs    map[*font.Glyph]rect
	upload    []byte

	fonts      *font.Set
	fontKey    string
	pixW, pixH int
	focused    bool
	title      string
	drawn      bool // painted since the last present
	preedit    paint.Preedit
	textInput  bool           // SDL text input (and so the input method) is on
	vars       map[string]any // g:ng_* variables
	imeArea    [5]int         // last area sent to SDL_SetTextInputArea
	settleAt   time.Time      // when to fit the grid to the window, if set
	reqW, reqH int            // window size last asked for, in pixels (0: none)
	wmSized    bool           // the window manager overrides ng's window sizes
	fitSentAt  time.Time      // when keepGridFitted last asked Neovim to resize

	verts []C.SDL_Vertex
	idx   []C.int

	pendingDrops []event // macOS sends the files the app is opened with before attaching

	attached     bool
	uiEntered    bool // Neovim's UIEnter has fired: the config has run
	flushed      bool // at least one complete screen has arrived
	shown        bool // the window is visible
	presented    bool
	gridCols     int // grid size that fits the window
	gridRows     int
	suppressText bool
	mouseButton  string
	mouseRow     int
	mouseCol     int

	trace      io.Writer
	traceStart time.Time
	onShown    func()
	latency    latency.Recorder // keystroke to frame, for --trace

	mu      sync.Mutex
	batches []batch
	ctl     []func() // work posted to the main thread
	closed  bool
	fatal   error

	restart *nvim.Restart // :restart is in progress (main thread)

	imageTex map[uint32]imageTexture // vim.ui.img images (main thread)

	busySince time.Time // when Neovim last became busy, zero when it is not
}

// batch is one redraw notification and when it arrived.
type batch struct {
	params  []any
	at      time.Time
	uiEnter bool   // the UIEnter notification, kept in order with redraws
	setSize []int  // columns, lines from SetSizeNotification
	varName string // VarNotification
	varVal  any
}

// tracef writes a timestamped line to the trace, if enabled.
func (a *App) tracef(at time.Time, format string, args ...any) {
	if a.trace == nil {
		return
	}
	ms := float64(at.Sub(a.traceStart).Microseconds()) / 1000
	fmt.Fprintf(a.trace, "%10.3f "+format+"\n", append([]any{ms}, args...)...)
}

// post runs f on the main thread. Safe to call from any goroutine.
func (a *App) post(f func()) {
	a.mu.Lock()
	a.ctl = append(a.ctl, f)
	a.mu.Unlock()
	wake()
}

// New prepares an App for a connection that has not attached yet. Redraw
// notifications must be passed to Enqueue.
func New() *App {
	return &App{glyphs: map[*font.Glyph]rect{}, focused: true, vars: map[string]any{},
		imageTex: map[uint32]imageTexture{}}
}

// Enqueue is the redraw notification handler. It runs on the RPC reader
// goroutine and only hands the batch to the main thread.
func (a *App) Enqueue(method string, params []any) {
	var b batch
	switch method {
	case "redraw":
		b = batch{params: params, at: time.Now()}
	case nvim.UIEnterNotification:
		b = batch{at: time.Now(), uiEnter: true}
	case nvim.VarNotification:
		if len(params) != 2 {
			return
		}
		name, _ := params[0].(string)
		b = batch{at: time.Now(), varName: name, varVal: params[1]}
	case nvim.SetSizeNotification:
		if len(params) != 2 {
			return
		}
		c, _ := msgpack.ToInt(params[0])
		r, _ := msgpack.ToInt(params[1])
		b = batch{at: time.Now(), setSize: []int{int(c), int(r)}}
	default:
		return
	}
	a.mu.Lock()
	a.batches = append(a.batches, b)
	a.mu.Unlock()
	wake()
}

func (a *App) markClosed() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	wake()
}

// Run connects to Neovim, creates the window and runs until Neovim closes
// the connection. It must be called from the main goroutine.
//
// Startup runs in parallel: Neovim starts (and reads the user's config once
// ng has attached), fontconfig loads its caches, and SDL creates the window
// and renderer, all at the same time. The window stays hidden until
// Neovim's UI has entered (the config has run) and the next screen is
// complete, so it appears once, with the fonts and size from the config.
func (a *App) Run(connect Connector, opt Options) error {
	a.trace, a.traceStart = opt.Trace, opt.Start
	a.onShown = opt.OnShown
	if a.traceStart.IsZero() {
		a.traceStart = time.Now()
	}
	a.mark("run")
	// Connect and attach right away, without waiting for the main thread
	// (busy creating the window): an embedded Neovim reads the user's
	// config only after a UI has attached, so attaching early lets the
	// config run while SDL initializes.
	a.session(connect, opt.Cols, opt.Rows, func() { a.post(a.showWaiting) }, nil)
	go font.Warmup()

	if !C.ng_init() {
		return sdlError("SDL_Init")
	}
	defer C.SDL_Quit()
	a.mark("sdl initialized (video driver " + C.GoString(C.SDL_GetCurrentVideoDriver()) + ")")

	title := C.CString("ng")
	a.win = C.ng_create_window(title, 800, 600)
	C.free(unsafe.Pointer(title))
	if a.win == nil {
		return sdlError("SDL_CreateWindow")
	}
	defer C.SDL_DestroyWindow(a.win)
	a.setIcon()
	a.mark("window created")
	a.ren = C.SDL_CreateRenderer(a.win, nil)
	if a.ren == nil {
		return sdlError("SDL_CreateRenderer")
	}
	defer C.SDL_DestroyRenderer(a.ren)
	if a.atlasTex = C.ng_create_texture(a.ren, C.SDL_TEXTUREACCESS_STATIC, atlasSize, atlasSize); a.atlasTex == nil {
		return sdlError("atlas texture")
	}
	a.packer = atlas.NewPacker(atlasSize, atlasSize)
	a.mark("renderer created (" + C.GoString(C.SDL_GetRendererName(a.ren)) + ")")

	a.scr = screen.New(opt.Cols, opt.Rows)
	a.gridCols, a.gridRows = opt.Cols, opt.Rows
	a.setTextInput(true)
	err := a.loop()
	if a.trace != nil {
		fmt.Fprintf(a.trace, "# latency (key event to present, inside ng)\n# %s\n",
			strings.ReplaceAll(latency.Summary(a.latency.Samples()), "\n", "\n# "))
	}
	return err
}

// session connects to Neovim and attaches ng's UI on a goroutine. setup,
// if not nil, runs after connecting and before attaching.
func (a *App) session(connect Connector, cols, rows int, onBlocked func(), setup func(*nvim.Nvim)) {
	go func() {
		nv, err := connect(a.Enqueue)
		connectedAt := time.Now()
		if err != nil {
			a.post(func() { a.fatal = err })
			return
		}
		go func() {
			nv.Serve()
			a.markClosed()
		}()
		a.post(func() { a.connected(nv, connectedAt) })
		if setup != nil {
			setup(nv)
		}
		err = nv.Prepare(bootstrap, cols, rows, onBlocked)
		attachedAt := time.Now()
		a.post(func() { a.attachDone(err, attachedAt) })
	}()
}

// restartSession attaches to the server :restart started, keeping the
// window: Neovim sent the "restart" event, then the old server exited.
func (a *App) restartSession() {
	r := a.restart
	a.restart = nil
	a.mu.Lock()
	a.closed = false
	a.mu.Unlock()
	a.attached = false
	a.vars = map[string]any{} // the bootstrap reports them again
	a.scr.ResetGraphics()     // the old server's images are gone with it
	a.updateTextInput()
	a.tracef(time.Now(), "restart attach to %s", r.Addr)
	connect := func(onNotify rpc.NotificationHandler) (*nvim.Nvim, error) {
		return nvim.Connect(r.Addr, onNotify, nil)
	}
	a.session(connect, a.gridCols, a.gridRows, nil, func(nv *nvim.Nvim) {
		// Older Neovim has no nvim_chan_set and sends no config.
		_ = nv.RestoreChannel(r.ChanConfig)
	})
}

// NvimAddr returns the address of the Neovim ng is connected to, or "".
func (a *App) NvimAddr() string {
	if a.nv == nil {
		return ""
	}
	return a.nv.Addr
}

// NvimDone is closed when the connection to Neovim ends (nil before it
// exists).
func (a *App) NvimDone() <-chan struct{} {
	if a.nv == nil {
		return nil
	}
	return a.nv.Done()
}

// mark records a startup milestone in the trace.
func (a *App) mark(what string) { a.markAt(time.Now(), what) }

// markAt records a milestone that happened at t (on another goroutine).
func (a *App) markAt(t time.Time, what string) {
	if a.trace != nil {
		a.tracef(t, "startup %s", what)
	}
}

// connected runs on the main thread once Neovim is reachable.
func (a *App) connected(nv *nvim.Nvim, at time.Time) {
	a.nv = nv
	a.markAt(at, "nvim connected")
}

// attachDone runs on the main thread when nvim.Prepare has finished.
func (a *App) attachDone(err error, at time.Time) {
	if err != nil {
		a.fatal = fmt.Errorf("attach: %w", err)
		return
	}
	a.attached = true
	a.markAt(at, "attached")
	for _, e := range a.pendingDrops {
		a.handle(e)
	}
	a.pendingDrops = nil
	if a.shown && (a.gridCols != a.scr.Width || a.gridRows != a.scr.Height) {
		a.nv.TryResize(a.gridCols, a.gridRows)
	}
	// Errors from startup files that Neovim could not show in a UI.
	a.nv.ReportStartupErrors()
	go a.nv.WarnIfUnsupported()
}

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

// showTimeout is how long ng waits for UIEnter before showing the window.
const showTimeout = 2 * time.Second

// Frame coalescing: see package frame.
const (
	quietMs    = int(frame.Quiet / time.Millisecond)
	maxDelayMs = frame.MaxDelay
)

func (a *App) loop() error {
	var ev C.SDL_Event
	for {
		if !a.shown {
			// Show the window even if UIEnter never comes (an old Neovim,
			// or a config that never finishes), so the user is not left
			// with nothing.
			if time.Since(a.traceStart) > showTimeout && a.flushed {
				a.show()
				a.paintDirty()
				a.present()
			}
		}
		if !a.settleAt.IsZero() && !time.Now().Before(a.settleAt) {
			a.settle()
			a.present()
		}
		got := true
		switch {
		case !a.shown:
			// Wake up regularly while hidden, for the timeout above.
			got = C.ng_wait_timeout(&ev, 50) != 0
		case !a.settleAt.IsZero():
			ms := max(1, int(time.Until(a.settleAt)/time.Millisecond)+1)
			got = C.ng_wait_timeout(&ev, C.int(ms)) != 0
		case a.scr.Busy && !a.busySince.IsZero() && !a.busyHidesCursor(time.Now()):
			// Busy, cursor still shown: wake up when it is to be hidden.
			ms := max(1, int(time.Until(a.busySince.Add(busyDelay))/time.Millisecond)+1)
			if got = C.ng_wait_timeout(&ev, C.int(ms)) != 0; !got && a.busyHidesCursor(time.Now()) {
				a.drawn = true
			}
		default:
			if C.ng_wait(&ev) == 0 {
				return sdlError("SDL_WaitEvent")
			}
		}
		evp := &ev
		if !got {
			evp = nil // timed out: only process queued redraws
		}
		present, closed, err := a.drain(evp)
		if err != nil || closed {
			return err
		}
		if a.drawn {
			deadline := time.Now().Add(maxDelayMs)
			for time.Now().Before(deadline) && C.ng_wait_timeout(&ev, C.int(quietMs)) != 0 {
				p, closed, err := a.drain(&ev)
				if err != nil || closed {
					return err
				}
				present = present || p
			}
		}
		if a.drawn || present {
			a.drawn = false
			a.present()
		}
	}
}

// drain handles ev and every other pending event, then applies the queued
// redraws. Painting into the screen texture happens at each flush; only
// presenting is deferred.
func (a *App) drain(ev *C.SDL_Event) (present, closed bool, err error) {
	wakeType := uint32(C.ng_get_wake_type())
	for ev != nil {
		e := flatten(ev)
		if e.typ != wakeType && a.handle(e) {
			present = true
		}
		if C.ng_poll(ev) == 0 {
			break
		}
	}
	drawn, closed, err := a.processRedraws()
	a.drawn = a.drawn || drawn
	return present, closed, err
}

// processRedraws applies queued redraw batches and paints at each flush.
func (a *App) processRedraws() (drawn, closed bool, err error) {
	a.mu.Lock()
	batches, ctl := a.batches, a.ctl
	a.batches, a.ctl = nil, nil
	closed = a.closed
	a.mu.Unlock()
	for _, f := range ctl {
		f()
	}
	if a.fatal != nil {
		return drawn, closed, a.fatal
	}
	for _, b := range batches {
		if b.varName != "" {
			a.tracef(b.at, "var     g:%s = %v", b.varName, b.varVal)
			if b.varVal == nil {
				delete(a.vars, b.varName)
			} else {
				a.vars[b.varName] = b.varVal
			}
			a.updateTextInput()
			continue
		}
		if b.setSize != nil {
			a.tracef(b.at, "set size %dx%d (:set columns/lines)", b.setSize[0], b.setSize[1])
			if a.shown && a.fonts != nil {
				// Like GVim: the window follows 'columns' and 'lines'.
				a.resizeWindowToGrid(b.setSize[0], b.setSize[1])
			}
			continue
		}
		if b.uiEnter {
			a.tracef(b.at, "startup UIEnter (config has run)")
			a.uiEntered = true
			a.nv.EnableTTY() // for vim.ui.img; see its comment
			if a.flushed && !a.shown {
				// Nothing may change after UIEnter: show what we have.
				a.show()
				a.paintDirty()
				drawn = true
			}
			continue
		}
		if a.trace != nil {
			a.tracef(b.at, "redraw  %s", screen.Describe(b.params))
		}
		if r := nvim.ParseRestart(b.params); r != nil {
			// The old server exits next; attach to r once it has.
			a.restart = r
		}
		flush, err := a.scr.HandleRedraw(b.params)
		if err != nil {
			return drawn, closed, err
		}
		// Replies to what Neovim wrote to the "terminal" (vim.ui.img asks
		// whether images are supported), and images placed or removed:
		// those need a present even if no row changed.
		for _, r := range a.scr.Graphics.TakeResponses() {
			a.tracef(b.at, "termresponse %q", r)
			a.nv.TermResponse(r)
		}
		if a.scr.Graphics.TakeChanged() && a.shown {
			drawn = true
		}
		switch {
		case a.scr.Busy && a.busySince.IsZero():
			a.busySince = b.at
		case !a.scr.Busy:
			a.busySince = time.Time{}
		}
		if flush {
			a.flushed = true
			a.latency.Flush(b.at)
			a.applyUIChanges()
			a.keepGridFitted()
			a.updateTextInput()
			if a.uiEntered && !a.shown {
				a.show()
			}
			a.paintDirty()
			drawn = true
		}
	}
	if closed && a.restart != nil {
		a.restartSession()
		closed = false
	}
	return drawn, closed, nil
}

// applyUIChanges reacts to option_set and set_title after a flush.
func (a *App) applyUIChanges() {
	changed := a.scr.TakeOptionChanges()
	if slices.Contains(changed, "guifont") || slices.Contains(changed, "guifontwide") ||
		slices.Contains(changed, "linespace") {
		gf, gfw := a.scr.OptionString("guifont"), a.scr.OptionString("guifontwide")
		if err := a.loadFonts(gf, gfw, a.scr.OptionInt("linespace")); err != nil {
			a.errorMessage(err.Error())
		} else if a.shown {
			// Like GVim: keep 'lines' and 'columns' and resize the window.
			a.resizeWindowToGrid(a.scr.Width, a.scr.Height)
		}
	}
	if a.scr.TakeColorsChanged() {
		a.scr.MarkAllDirty()
	}
	if a.scr.Title != a.title {
		a.title = a.scr.Title
		t := C.CString(a.title)
		C.SDL_SetWindowTitle(a.win, t)
		C.free(unsafe.Pointer(t))
	}
}

func (a *App) errorMessage(msg string) {
	fmt.Fprintln(os.Stderr, "ng:", msg)
	a.nv.Notify("nvim_echo", []any{[]any{"ng: " + msg, "ErrorMsg"}}, true, map[string]any{})
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

// loadFonts replaces the font set. The atlas is emptied and every row is
// repainted.
func (a *App) loadFonts(guifont, guifontwide string, linespace int) error {
	dpi := a.dpi()
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%f", guifont, guifontwide, linespace, dpi)
	if key == a.fontKey && a.fonts != nil {
		return nil
	}
	f, err := font.NewSet(guifont, guifontwide, dpi, linespace)
	if err != nil {
		return err
	}
	for _, w := range f.Warnings {
		a.errorMessage(w)
	}
	if a.fonts != nil {
		a.fonts.Close()
	}
	a.fonts, a.fontKey = f, key
	a.resetAtlas()
	a.scr.MarkAllDirty()
	return nil
}

func (a *App) resetAtlas() {
	clear(a.glyphs)
	a.packer.Reset()
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
		if err := a.loadFonts(a.scr.OptionString("guifont"), a.scr.OptionString("guifontwide"),
			a.scr.OptionInt("linespace")); err != nil {
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

// ensureScreenTexture (re)creates the texture that holds the painted grid.
// It reports whether the texture is new and must be painted completely.
func (a *App) ensureScreenTexture() bool {
	if a.screenTex != nil && a.texW == a.pixW && a.texH == a.pixH {
		return false
	}
	if a.screenTex != nil {
		C.SDL_DestroyTexture(a.screenTex)
	}
	a.screenTex = C.ng_create_texture(a.ren, C.SDL_TEXTUREACCESS_TARGET, C.int(max(1, a.pixW)), C.int(max(1, a.pixH)))
	a.texW, a.texH = a.pixW, a.pixH
	return true
}

// paintDirty repaints the changed rows into the screen texture.
func (a *App) paintDirty() {
	if !a.shown || a.fonts == nil {
		return
	}
	full := a.ensureScreenTexture()
	if a.screenTex == nil {
		return
	}
	if full {
		a.scr.MarkAllDirty()
	}
	rows := a.scr.TakeDirty()
	C.SDL_SetRenderTarget(a.ren, a.screenTex)
	if full || len(rows) == a.scr.Height {
		paint.Padding(a, a.scr, a.fonts, a.pixW, a.pixH)
	}
	for _, r := range rows {
		paint.Row(a, a.scr, a.fonts, r)
	}
	a.flushGlyphs()
	C.SDL_SetRenderTarget(a.ren, nil)
}

// present shows the screen texture and the cursor on top of it.
func (a *App) present() {
	if a.screenTex == nil || !a.shown {
		return
	}
	if !a.presented {
		a.presented = true
		a.mark("first frame presented")
	}
	C.ng_blit(a.ren, a.screenTex, C.int(a.pixW), C.int(a.pixH))
	a.dropImages()
	paint.Images(a, a.scr, a.fonts)
	if !a.busyHidesCursor(time.Now()) {
		paint.Cursor(a, a.scr, a.fonts, a.focused)
	}
	area, cursorX := paint.CursorRect(a.scr, a.fonts), 0
	if a.preedit.Text != "" {
		area, cursorX = paint.DrawPreedit(a, a.scr, a.fonts, a.preedit)
		cursorX -= area.X
	}
	a.flushGlyphs()
	a.setIMEArea(area, cursorX)
	C.SDL_RenderPresent(a.ren)
	if a.trace != nil {
		if l, ok := a.latency.Present(time.Now()); ok {
			a.tracef(time.Now(), "latency %.2f ms for %s (queue %.2f, nvim %.2f, frame %.2f)",
				msf(l.Total()), l.Key, msf(l.Queue), msf(l.Nvim), msf(l.Frame))
		}
		m := a.scr.CurrentMode()
		a.tracef(time.Now(), "present cursor=(%d,%d) mode=%s busy=%v",
			a.scr.CursorRow, a.scr.CursorCol, m.Name, a.scr.Busy)
	}
}

// busyDelay is how long Neovim must stay busy before ng hides the
// cursor. Neovim sends busy_start and busy_stop around short pieces of
// work too, for example for every key typed on the command line with ui2.
// A terminal hides the cursor for those as well, but it shows the screen
// only at its refresh, so a busy period of a few milliseconds never
// appears; ng presents frames in between and the cursor blinked away.
const busyDelay = 100 * time.Millisecond

// busyHidesCursor reports whether the cursor is hidden at now because
// Neovim has been busy for at least busyDelay.
func (a *App) busyHidesCursor(now time.Time) bool {
	return a.scr.Busy && !a.busySince.IsZero() && now.Sub(a.busySince) >= busyDelay
}

// imeInsertOnly is g:ng_ime_insert_only, true unless set to 0 or false.
func (a *App) imeInsertOnly() bool {
	switch v := a.vars["ng_ime_insert_only"].(type) {
	case int64:
		return v != 0
	case bool:
		return v
	}
	return true
}

// updateTextInput turns SDL text input, and with it the input method, on
// in modes where text is typed and off elsewhere (see input.IMEWanted).
func (a *App) updateTextInput() {
	if want := input.IMEWanted(a.scr.ModeName, a.imeInsertOnly()); want != a.textInput {
		a.setTextInput(want)
	}
}

func (a *App) setTextInput(on bool) {
	if on {
		C.SDL_StartTextInput(a.win)
	} else {
		C.SDL_StopTextInput(a.win)
		a.preedit = paint.Preedit{}
	}
	a.textInput = on
	a.imeArea = [5]int{} // send the input area again
	if a.trace != nil {
		a.tracef(time.Now(), "ime     text input %v (mode %s)", on, a.scr.ModeName)
	}
}

// setIMEArea tells the input method where the cursor (or the text being
// composed) is, so that its candidate window appears there. Only changes
// are sent.
func (a *App) setIMEArea(r paint.Rect, cursorX int) {
	d := float64(C.ng_pixel_density(a.win))
	pt := func(v int) int { return int(float64(v) / d) }
	v := [5]int{pt(r.X), pt(r.Y), max(1, pt(r.W)), max(1, pt(r.H)), pt(cursorX)}
	if v == a.imeArea {
		return
	}
	a.imeArea = v
	C.ng_set_text_input_area(a.win, C.int(v[0]), C.int(v[1]), C.int(v[2]), C.int(v[3]), C.int(v[4]))
}

// FillRect implements paint.Painter.
func (a *App) FillRect(x, y, w, h int, c screen.Color) {
	C.ng_fill(a.ren, C.int(x), C.int(y), C.int(w), C.int(h), C.Uint32(c))
}

// DrawGlyph implements paint.Painter. Glyph quads are batched and drawn
// with one SDL_RenderGeometry call per flushGlyphs.
func (a *App) DrawGlyph(g *font.Glyph, x, y int, c screen.Color) {
	r, ok := a.glyphs[g]
	if !ok {
		px, py, fits := a.packer.Alloc(g.W, g.H)
		if !fits {
			// Atlas full: draw what is queued, then start over.
			a.flushGlyphs()
			a.resetAtlas()
			if px, py, fits = a.packer.Alloc(g.W, g.H); !fits {
				return
			}
		}
		a.upload = atlas.AlphaToRGBA(g.Pix, g.W, g.H, g.Stride, a.upload)
		C.ng_update(a.atlasTex, C.int(px), C.int(py), C.int(g.W), C.int(g.H),
			unsafe.Pointer(&a.upload[0]), C.int(g.W*4))
		r = rect{px, py, g.W, g.H}
		a.glyphs[g] = r
	}
	cr, cg, cb := c.RGB()
	col := C.SDL_FColor{r: C.float(cr) / 255, g: C.float(cg) / 255, b: C.float(cb) / 255, a: 1}
	x0, y0 := C.float(x), C.float(y)
	x1, y1 := C.float(x+r.w), C.float(y+r.h)
	u0, v0 := C.float(r.x)/atlasSize, C.float(r.y)/atlasSize
	u1, v1 := C.float(r.x+r.w)/atlasSize, C.float(r.y+r.h)/atlasSize
	base := C.int(len(a.verts))
	a.verts = append(a.verts,
		C.SDL_Vertex{position: C.SDL_FPoint{x: x0, y: y0}, color: col, tex_coord: C.SDL_FPoint{x: u0, y: v0}},
		C.SDL_Vertex{position: C.SDL_FPoint{x: x1, y: y0}, color: col, tex_coord: C.SDL_FPoint{x: u1, y: v0}},
		C.SDL_Vertex{position: C.SDL_FPoint{x: x1, y: y1}, color: col, tex_coord: C.SDL_FPoint{x: u1, y: v1}},
		C.SDL_Vertex{position: C.SDL_FPoint{x: x0, y: y1}, color: col, tex_coord: C.SDL_FPoint{x: u0, y: v1}},
	)
	a.idx = append(a.idx, base, base+1, base+2, base, base+2, base+3)
}

func (a *App) flushGlyphs() {
	if len(a.idx) == 0 {
		return
	}
	C.SDL_RenderGeometry(a.ren, a.atlasTex, &a.verts[0], C.int(len(a.verts)), &a.idx[0], C.int(len(a.idx)))
	a.verts, a.idx = a.verts[:0], a.idx[:0]
}

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
		if err := a.loadFonts(a.scr.OptionString("guifont"), a.scr.OptionString("guifontwide"),
			a.scr.OptionInt("linespace")); err == nil {
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

// leavingTextMode turns the input method off as soon as a key that leaves
// Insert mode is pressed, before Neovim reports the mode change: otherwise
// a key typed right after <Esc> could still be taken by the input method.
// If the mode does not change (Terminal mode, an <Esc> mapping), the next
// flush turns it on again.
func (a *App) leavingTextMode() {
	if a.textInput && a.imeInsertOnly() {
		a.setTextInput(false)
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

// msf is d in milliseconds, for traces.
func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
