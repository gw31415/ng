// C helpers for ng's SDL3 window. Kept in C because SDL_Event is a union
// and several SDL macros are not usable from cgo directly.
#include <string.h>
#include "sdl.h"



void ng_flatten(const SDL_Event *e, ng_event *o) {
	memset(o, 0, sizeof(*o));
	o->type = e->type;
	Uint64 now = SDL_GetTicksNS();
	o->age_ns = now > e->common.timestamp ? now - e->common.timestamp : 0;
	switch (e->type) {
	case SDL_EVENT_KEY_DOWN:
	case SDL_EVENT_KEY_UP:
		o->key = e->key.key;
		o->mod = e->key.mod;
		// e->key.key does not include Shift (Shift+; is ';'). Ask the
		// keyboard layout what the key types with the modifiers that
		// change characters.
		o->typed = SDL_GetKeyFromScancode(e->key.scancode,
			e->key.mod & (SDL_KMOD_SHIFT | SDL_KMOD_CAPS | SDL_KMOD_MODE), false);
		o->repeat = e->key.repeat;
		break;
	case SDL_EVENT_TEXT_EDITING:
		o->edit_start = e->edit.start;
		o->edit_length = e->edit.length;
		break;
	case SDL_EVENT_MOUSE_BUTTON_DOWN:
	case SDL_EVENT_MOUSE_BUTTON_UP:
		o->x = e->button.x;
		o->y = e->button.y;
		o->button = e->button.button;
		break;
	case SDL_EVENT_MOUSE_MOTION:
		o->x = e->motion.x;
		o->y = e->motion.y;
		o->state = e->motion.state;
		break;
	case SDL_EVENT_MOUSE_WHEEL:
		o->x = e->wheel.x;
		o->y = e->wheel.y;
		if (e->wheel.direction == SDL_MOUSEWHEEL_FLIPPED) {
			o->x = -o->x;
			o->y = -o->y;
		}
		break;
	}
}

// Drop events carry a file name that may be longer than ng_event.text.
char *ng_drop_data(const SDL_Event *e) {
	return e->drop.data ? SDL_strdup(e->drop.data) : NULL;
}

// Text of a text input (committed) or text editing (preedit) event, of
// any length (an input method may commit a whole sentence).
char *ng_event_text(const SDL_Event *e) {
	const char *t = NULL;
	if (e->type == SDL_EVENT_TEXT_INPUT)
		t = e->text.text;
	else if (e->type == SDL_EVENT_TEXT_EDITING)
		t = e->edit.text;
	return t ? SDL_strdup(t) : NULL;
}

// Tells the input method where the text being composed is, in window
// coordinates, so that it puts its candidate window there. cursor is the
// offset of the text cursor from x.
bool ng_set_text_input_area(SDL_Window *w, int x, int y, int width, int height, int cursor) {
	SDL_Rect r = { x, y, width, height };
	return SDL_SetTextInputArea(w, &r, cursor);
}

static Uint32 ng_wake_type = 0;

bool ng_init(void) {
	// The identifier becomes the Wayland app_id and the X11 WM_CLASS.
	SDL_SetAppMetadata("ng", NULL, "ng");
	// Prefer Wayland, then X11. SDL may otherwise pick XWayland on
	// compositors without the FIFO protocols (a concern for games that
	// present with vsync, which ng does not), and XWayland made SDL's
	// startup about 100 ms slower in traces. A SDL_VIDEO_DRIVER
	// environment variable still takes precedence over this hint.
#ifdef __linux__
	SDL_SetHint(SDL_HINT_VIDEO_DRIVER, "wayland,x11");
#endif
	// ng draws the preedit text itself, in the grid with its own CJK
	// rendering; the input method still draws its candidate window.
	SDL_SetHint(SDL_HINT_IME_IMPLEMENTED_UI, "composition");
	if (!SDL_Init(SDL_INIT_VIDEO))
		return false;
	ng_wake_type = SDL_RegisterEvents(1);
	return ng_wake_type != 0;
}

// ng_wake may be called from any thread.
void ng_wake(void) {
	SDL_Event e;
	SDL_zero(e);
	e.type = ng_wake_type;
	SDL_PushEvent(&e);
}

Uint32 ng_get_wake_type(void) { return ng_wake_type; }

int ng_wait(SDL_Event *e) { return SDL_WaitEvent(e) ? 1 : 0; }
int ng_poll(SDL_Event *e) { return SDL_PollEvent(e) ? 1 : 0; }
int ng_wait_timeout(SDL_Event *e, int ms) { return SDL_WaitEventTimeout(e, ms) ? 1 : 0; }

SDL_Window *ng_create_window(const char *title, int w, int h) {
	return SDL_CreateWindow(title, w, h,
		SDL_WINDOW_RESIZABLE | SDL_WINDOW_HIGH_PIXEL_DENSITY | SDL_WINDOW_HIDDEN);
}

void ng_fill(SDL_Renderer *r, int x, int y, int w, int h, Uint32 rgb) {
	SDL_SetRenderDrawColor(r, (rgb >> 16) & 0xff, (rgb >> 8) & 0xff, rgb & 0xff, 0xff);
	SDL_FRect rect = { (float)x, (float)y, (float)w, (float)h };
	SDL_RenderFillRect(r, &rect);
}

SDL_Texture *ng_create_texture(SDL_Renderer *r, int access, int w, int h) {
	SDL_Texture *t = SDL_CreateTexture(r, SDL_PIXELFORMAT_RGBA32, access, w, h);
	if (t) {
		SDL_SetTextureBlendMode(t, SDL_BLENDMODE_BLEND);
		SDL_SetTextureScaleMode(t, SDL_SCALEMODE_NEAREST);
	}
	return t;
}

bool ng_update(SDL_Texture *t, int x, int y, int w, int h, const void *pix, int pitch) {
	SDL_Rect rect = { x, y, w, h };
	return SDL_UpdateTexture(t, &rect, pix, pitch);
}

bool ng_blit(SDL_Renderer *r, SDL_Texture *t, int w, int h) {
	SDL_FRect rect = { 0, 0, (float)w, (float)h };
	SDL_SetTextureBlendMode(t, SDL_BLENDMODE_NONE);
	bool ok = SDL_RenderTexture(r, t, &rect, &rect);
	SDL_SetTextureBlendMode(t, SDL_BLENDMODE_BLEND);
	return ok;
}

float ng_pixel_density(SDL_Window *w) {
	float d = SDL_GetWindowPixelDensity(w);
	return d > 0 ? d : 1.0f;
}

// Returns the clipboard text (free with SDL_free), or NULL.
char *ng_clipboard_text(void) {
	if (!SDL_HasClipboardText())
		return NULL;
	return SDL_GetClipboardText();
}

// ng_set_icon sets the window icon from non-premultiplied RGBA pixels.
// SDL copies them.
bool ng_set_icon(SDL_Window *w, int width, int height, void *rgba) {
#ifdef __APPLE__
	return true; // SDL would replace the Dock icon of ng.app with this one
#endif
	SDL_Surface *s = SDL_CreateSurfaceFrom(width, height, SDL_PIXELFORMAT_RGBA32, rgba, width * 4);
	if (!s) {
		return false;
	}
	bool ok = SDL_SetWindowIcon(w, s);
	SDL_DestroySurface(s);
	return ok;
}
