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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Shougo/ng/internal/atlas"
	"github.com/Shougo/ng/internal/font"
	"github.com/Shougo/ng/internal/frame"
	"github.com/Shougo/ng/internal/latency"
	"github.com/Shougo/ng/internal/msgpack"
	"github.com/Shougo/ng/internal/nvim"
	"github.com/Shougo/ng/internal/paint"
	"github.com/Shougo/ng/internal/rpc"
	"github.com/Shougo/ng/internal/screen"
)

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

// showTimeout is how long ng waits for UIEnter before showing the window.
const showTimeout = 2 * time.Second

// Frame coalescing: see package frame.
const (
	quietMs  = int(frame.Quiet / time.Millisecond)
	maxDelay = frame.MaxDelay
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
			deadline := time.Now().Add(maxDelay)
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
		if err := a.loadOptionFonts(); err != nil {
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

// msf is d in milliseconds, for traces.
func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
