// Package font loads fonts with fontconfig and rasterizes glyphs with
// FreeType. FreeType reads TrueType, OpenType, Type 1 and bitmap fonts, so
// fonts that Pango no longer accepts (for example the Type 1 Courier 10
// Pitch) work here unchanged.
//
// Every glyph is fitted into the cells Neovim assigned to it: a glyph that
// is wider or taller than its cells is scaled down and centered instead of
// spilling into its neighbours. That keeps ambiguous-width characters and
// fallback fonts from leaving fragments on the screen.
package font

/*
#cgo pkg-config: freetype2 fontconfig
#include <stdlib.h>
#include <string.h>
#include <ft2build.h>
#include FT_FREETYPE_H
#include FT_SYNTHESIS_H
#include <fontconfig/fontconfig.h>

typedef struct {
	char *path;
	int index;
	int weight;
	int slant;
	int hintstyle;  // FC_HINT_NONE .. FC_HINT_FULL, -1 if unset
	int hinting;    // -1 if unset
	int antialias;  // -1 if unset
	int autohint;   // -1 if unset
	int family_ok;  // matched family equals the requested one
} ng_match_t;

static int ng_get_int(FcPattern *p, const char *obj, int def) {
	int v;
	return FcPatternGetInteger(p, obj, 0, &v) == FcResultMatch ? v : def;
}

static int ng_get_bool(FcPattern *p, const char *obj) {
	FcBool v;
	return FcPatternGetBool(p, obj, 0, &v) == FcResultMatch ? (int)v : -1;
}

static void ng_fill(FcPattern *font, const char *family, ng_match_t *out) {
	FcChar8 *file = NULL, *fam = NULL;
	FcPatternGetString(font, FC_FILE, 0, &file);
	out->path = file ? strdup((const char *)file) : NULL;
	out->index = ng_get_int(font, FC_INDEX, 0);
	out->weight = ng_get_int(font, FC_WEIGHT, FC_WEIGHT_REGULAR);
	out->slant = ng_get_int(font, FC_SLANT, FC_SLANT_ROMAN);
	out->hintstyle = ng_get_int(font, FC_HINT_STYLE, -1);
	out->hinting = ng_get_bool(font, FC_HINTING);
	out->antialias = ng_get_bool(font, FC_ANTIALIAS);
	out->autohint = ng_get_bool(font, FC_AUTOHINT);
	out->family_ok = 0;
	if (family) {
		for (int i = 0; FcPatternGetString(font, FC_FAMILY, i, &fam) == FcResultMatch; i++) {
			if (FcStrCmpIgnoreCase(fam, (const FcChar8 *)family) == 0) {
				out->family_ok = 1;
				break;
			}
		}
	}
}

// ng_match finds a font. family may be NULL. When ch is non-zero only a
// font that contains ch is accepted (used for fallback lookup).
static int ng_match(const char *family, int bold, int italic, unsigned int ch,
                    ng_match_t *out) {
	FcPattern *pat = FcPatternCreate();
	if (family)
		FcPatternAddString(pat, FC_FAMILY, (const FcChar8 *)family);
	FcPatternAddInteger(pat, FC_WEIGHT, bold ? FC_WEIGHT_BOLD : FC_WEIGHT_REGULAR);
	FcPatternAddInteger(pat, FC_SLANT, italic ? FC_SLANT_ITALIC : FC_SLANT_ROMAN);
	FcPatternAddInteger(pat, FC_SPACING, FC_MONO);
	FcCharSet *cs = NULL;
	if (ch) {
		cs = FcCharSetCreate();
		FcCharSetAddChar(cs, ch);
		FcPatternAddCharSet(pat, FC_CHARSET, cs);
	}
	FcConfigSubstitute(NULL, pat, FcMatchPattern);
	FcDefaultSubstitute(pat);

	int ok = 0;
	FcResult res;
	if (!ch) {
		FcPattern *m = FcFontMatch(NULL, pat, &res);
		if (m) {
			ng_fill(m, family, out);
			ok = out->path != NULL;
			FcPatternDestroy(m);
		}
	} else {
		FcFontSet *set = FcFontSort(NULL, pat, FcFalse, NULL, &res);
		for (int i = 0; set && i < set->nfont && !ok; i++) {
			FcCharSet *fcs;
			if (FcPatternGetCharSet(set->fonts[i], FC_CHARSET, 0, &fcs) != FcResultMatch)
				continue;
			if (!FcCharSetHasChar(fcs, ch))
				continue;
			FcPattern *m = FcFontRenderPrepare(NULL, pat, set->fonts[i]);
			if (m) {
				ng_fill(m, family, out);
				ok = out->path != NULL;
				FcPatternDestroy(m);
			}
		}
		if (set)
			FcFontSetDestroy(set);
		FcCharSetDestroy(cs);
	}
	FcPatternDestroy(pat);
	return ok;
}

static int ng_ft_load(FT_Face face, unsigned int gi, int flags, int embolden, int oblique) {
	int err = FT_Load_Glyph(face, gi, flags);
	if (err)
		return err;
	if (face->glyph->format == FT_GLYPH_FORMAT_OUTLINE) {
		if (embolden)
			FT_GlyphSlot_Embolden(face->glyph);
		if (oblique)
			FT_GlyphSlot_Oblique(face->glyph);
	}
	return 0;
}

static int ng_ft_render(FT_Face face, int mono) {
	if (face->glyph->format == FT_GLYPH_FORMAT_BITMAP &&
	    face->glyph->bitmap.pixel_mode != FT_PIXEL_MODE_MONO)
		return 0;
	return FT_Render_Glyph(face->glyph, mono ? FT_RENDER_MODE_MONO : FT_RENDER_MODE_NORMAL);
}

enum {
	ng_load_default = FT_LOAD_DEFAULT,
	ng_load_no_hinting = FT_LOAD_NO_HINTING,
	ng_load_force_autohint = FT_LOAD_FORCE_AUTOHINT,
	ng_load_target_mono = FT_LOAD_TARGET_MONO,
	ng_load_target_light = FT_LOAD_TARGET_LIGHT,
	ng_load_target_normal = FT_LOAD_TARGET_NORMAL,
};

static FT_GlyphSlot ng_slot(FT_Face f) { return f->glyph; }
static long ng_ascender(FT_Face f) { return f->size->metrics.ascender; }
static long ng_descender(FT_Face f) { return f->size->metrics.descender; }
static int ng_scalable(FT_Face f) { return FT_IS_SCALABLE(f) ? 1 : 0; }
static int ng_num_fixed(FT_Face f) { return f->num_fixed_sizes; }
static int ng_fixed_height(FT_Face f, int i) { return f->available_sizes[i].height; }
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"
	"unsafe"
)

// Style selects a font variant.
type Style uint8

const (
	Bold Style = 1 << iota
	Italic
)

// Glyph is an 8-bit coverage bitmap positioned relative to the top-left
// corner of its cells.
type Glyph struct {
	X, Y   int // offset of the bitmap from the cell origin
	W, H   int
	Stride int
	Pix    []byte // W*H alpha values when Stride == W
}

// Empty reports whether the glyph has no pixels (for example a space).
func (g *Glyph) Empty() bool { return g.W == 0 || g.H == 0 }

type face struct {
	ft        C.FT_Face
	path      string
	index     int
	base      float64 // configured size in points
	sizeSet   float64 // last size passed to FT_Set_Char_Size
	loadFlags C.int
	mono      bool
	embolden  bool
	oblique   bool
}

type faceKey struct {
	path  string
	index int
	style Style
	size  float64
}

type glyphKey struct {
	text  string
	style Style
	cells int
}

// Set is a configured group of fonts: the 'guifont' list, the optional
// 'guifontwide' list and automatic fallbacks. It is not safe for concurrent
// use.
type Set struct {
	lib     C.FT_Library
	size    float64 // points
	dpi     float64
	spec    Spec
	wide    Spec
	faces   map[faceKey]*face
	byStyle map[Style][]*face // guifont faces, in order
	wideBy  map[Style][]*face // guifontwide faces
	fall    map[fallKey]*face
	glyphs  map[glyphKey]*Glyph

	// Cell geometry in pixels.
	CellW, CellH int
	Baseline     int // distance from the top of a cell to the baseline
	Warnings     []string
}

type fallKey struct {
	r     rune
	style Style
}

// fcInit initializes fontconfig once. Loading its caches takes a while,
// so ng starts it early on another goroutine (Warmup).
var fcInit = sync.OnceValue(func() error {
	if C.FcInit() == 0 {
		return errors.New("font: FcInit failed")
	}
	return nil
})

// Warmup initializes fontconfig ahead of the first NewSet. It is safe to
// call from any goroutine.
func Warmup() { fcInit() }

// NewSet loads the fonts named by guifont and guifontwide at the given DPI.
// linespace adds pixels to every row, like the 'linespace' option.
func NewSet(guifont, guifontwide string, dpi float64, linespace int) (*Set, error) {
	if err := fcInit(); err != nil {
		return nil, err
	}
	s := &Set{
		dpi:     dpi,
		spec:    ParseSpec(guifont),
		wide:    ParseSpec(guifontwide),
		faces:   map[faceKey]*face{},
		byStyle: map[Style][]*face{},
		wideBy:  map[Style][]*face{},
		fall:    map[fallKey]*face{},
		glyphs:  map[glyphKey]*Glyph{},
	}
	if C.FT_Init_FreeType(&s.lib) != 0 {
		return nil, errors.New("font: FT_Init_FreeType failed")
	}
	s.size = s.spec.Size
	if s.size == 0 {
		s.size = s.wide.Size
	}
	if s.size == 0 {
		s.size = DefaultSize
	}
	if len(s.spec.Families) == 0 {
		s.spec.Families = []string{"monospace"}
	}
	regular := s.facesFor(s.spec.Families, 0, s.size)
	if len(regular) == 0 {
		s.Close()
		return nil, fmt.Errorf("font: no usable font for %q", guifont)
	}
	if err := s.measure(regular[0], linespace); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close frees all FreeType resources.
func (s *Set) Close() {
	for _, f := range s.faces {
		C.FT_Done_Face(f.ft)
	}
	s.faces = nil
	if s.lib != nil {
		C.FT_Done_FreeType(s.lib)
		s.lib = nil
	}
}

// Size returns the font size in points.
func (s *Set) Size() float64 { return s.size }

func (s *Set) facesFor(families []string, style Style, size float64) []*face {
	var out []*face
	for _, fam := range families {
		f, err := s.load(fam, style, 0, size)
		if err != nil {
			if style == 0 {
				s.Warnings = append(s.Warnings, err.Error())
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

func (s *Set) primaries(style Style) []*face {
	if fs, ok := s.byStyle[style]; ok {
		return fs
	}
	fs := s.facesFor(s.spec.Families, style, s.size)
	s.byStyle[style] = fs
	return fs
}

func (s *Set) wides(style Style) []*face {
	if fs, ok := s.wideBy[style]; ok {
		return fs
	}
	// 'guifontwide' may have its own size, as in GVim.
	size := s.wide.Size
	if size == 0 {
		size = s.size
	}
	fs := s.facesFor(s.wide.Families, style, size)
	s.wideBy[style] = fs
	return fs
}

// load opens the font fontconfig picks for family (or, with ch != 0 and an
// empty family, any font containing ch).
func (s *Set) load(family string, style Style, ch rune, size float64) (*face, error) {
	if a, ok := familyAliases[family]; ok {
		family = a
	}
	var cfam *C.char
	if family != "" {
		cfam = C.CString(family)
		defer C.free(unsafe.Pointer(cfam))
	}
	var m C.ng_match_t
	bold, italic := C.int(0), C.int(0)
	if style&Bold != 0 {
		bold = 1
	}
	if style&Italic != 0 {
		italic = 1
	}
	if C.ng_match(cfam, bold, italic, C.uint(ch), &m) == 0 {
		return nil, fmt.Errorf("font: no match for %q", family)
	}
	path := C.GoString(m.path)
	C.free(unsafe.Pointer(m.path))
	// A primary font that fontconfig silently replaced is not what the user
	// asked for; skip it so the next family in the list is used.
	if family != "" && ch == 0 && m.family_ok == 0 && family != "monospace" {
		return nil, fmt.Errorf("font: %q not found", family)
	}

	key := faceKey{path, int(m.index), style, size}
	if f, ok := s.faces[key]; ok {
		return f, nil
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var ft C.FT_Face
	if C.FT_New_Face(s.lib, cpath, C.FT_Long(m.index), &ft) != 0 {
		return nil, fmt.Errorf("font: cannot open %s", path)
	}
	f := &face{ft: ft, path: path, index: int(m.index), base: size}
	f.embolden = style&Bold != 0 && m.weight < C.FC_WEIGHT_DEMIBOLD
	f.oblique = style&Italic != 0 && m.slant == C.FC_SLANT_ROMAN
	f.loadFlags, f.mono = loadFlags(m)
	if err := s.setSize(f, size); err != nil {
		C.FT_Done_Face(ft)
		return nil, err
	}
	s.faces[key] = f
	return f, nil
}

// loadFlags honours the user's fontconfig rendering settings.
func loadFlags(m C.ng_match_t) (C.int, bool) {
	flags := C.int(C.ng_load_default)
	mono := m.antialias == 0
	switch {
	case m.hinting == 0 || m.hintstyle == C.FC_HINT_NONE:
		flags |= C.ng_load_no_hinting
	case mono:
		flags |= C.ng_load_target_mono
	case m.hintstyle == C.FC_HINT_SLIGHT || m.hintstyle == -1:
		flags |= C.ng_load_target_light
	default:
		flags |= C.ng_load_target_normal
	}
	if m.autohint == 1 {
		flags |= C.ng_load_force_autohint
	}
	return flags, mono
}

func (s *Set) setSize(f *face, pt float64) error {
	if f.sizeSet == pt {
		return nil
	}
	if C.ng_scalable(f.ft) != 0 {
		if C.FT_Set_Char_Size(f.ft, 0, C.FT_F26Dot6(math.Round(pt*64)),
			C.FT_UInt(s.dpi), C.FT_UInt(s.dpi)) != 0 {
			return fmt.Errorf("font: cannot set size of %s", f.path)
		}
	} else {
		// Bitmap font: pick the strike closest to the wanted pixel height.
		want := pt * s.dpi / 72
		best, bestDiff := 0, math.MaxFloat64
		for i := 0; i < int(C.ng_num_fixed(f.ft)); i++ {
			d := math.Abs(float64(C.ng_fixed_height(f.ft, C.int(i))) - want)
			if d < bestDiff {
				best, bestDiff = i, d
			}
		}
		if C.FT_Select_Size(f.ft, C.FT_Int(best)) != 0 {
			return fmt.Errorf("font: cannot select size of %s", f.path)
		}
	}
	f.sizeSet = pt
	return nil
}

func ceil26(v C.long) int  { return int((v + 63) >> 6) }
func floor26(v C.long) int { return int(v >> 6) }

func (s *Set) measure(f *face, linespace int) error {
	gi := C.FT_Get_Char_Index(f.ft, 'M')
	if gi == 0 {
		gi = C.FT_Get_Char_Index(f.ft, '0')
	}
	if C.ng_ft_load(f.ft, gi, f.loadFlags, 0, 0) != 0 {
		return fmt.Errorf("font: cannot measure %s", f.path)
	}
	adv := C.ng_slot(f.ft).advance.x
	s.CellW = max(1, int(math.Round(float64(adv)/64)))
	asc := ceil26(C.ng_ascender(f.ft))
	desc := -floor26(C.ng_descender(f.ft))
	s.CellH = max(1, asc+desc+linespace)
	s.Baseline = asc + linespace/2
	return nil
}

func hasChar(f *face, r rune) bool { return C.FT_Get_Char_Index(f.ft, C.FT_ULong(r)) != 0 }

// faceFor picks the font for r: 'guifontwide' for double-width cells, then
// 'guifont' in order, then a fontconfig fallback.
func (s *Set) faceFor(r rune, style Style, cells int) (f *face, primary bool) {
	if cells == 2 {
		for _, f := range s.wides(style) {
			if hasChar(f, r) {
				return f, false
			}
		}
	}
	for i, f := range s.primaries(style) {
		if hasChar(f, r) {
			return f, i == 0
		}
	}
	k := fallKey{r, style}
	if f, ok := s.fall[k]; ok {
		return f, false
	}
	f, err := s.load("", style, r, s.size)
	if err != nil {
		f = nil
	}
	s.fall[k] = f
	return f, false
}

// Glyph returns the bitmap for text drawn in cells cells. text is the
// content of one grid cell: a base character optionally followed by
// combining characters. The result is cached.
func (s *Set) Glyph(text string, style Style, cells int) *Glyph {
	k := glyphKey{text, style, cells}
	if g, ok := s.glyphs[k]; ok {
		return g
	}
	g := s.render(text, style, cells)
	s.glyphs[k] = g
	return g
}

func (s *Set) render(text string, style Style, cells int) *Glyph {
	empty := &Glyph{}
	base, _ := utf8.DecodeRuneInString(text)
	if base == utf8.RuneError || base == ' ' || text == "" {
		return empty
	}
	f, primary := s.faceFor(base, style, cells)
	if f == nil {
		return empty
	}
	availW := cells * s.CellW

	// Measure at the configured size, then shrink if the glyph does not fit
	// its cells. Glyphs from the primary font in a single cell are trusted
	// so that their hinting is not disturbed.
	scale := 1.0
	if !primary || cells != 1 {
		if err := s.setSize(f, f.base); err != nil {
			return empty
		}
		gi := C.FT_Get_Char_Index(f.ft, C.FT_ULong(base))
		if C.ng_ft_load(f.ft, gi, f.loadFlags, 0, 0) == 0 {
			adv := float64(C.ng_slot(f.ft).advance.x) / 64
			h := float64(C.ng_ascender(f.ft)-C.ng_descender(f.ft)) / 64
			if adv > float64(availW) {
				scale = math.Min(scale, float64(availW)/adv)
			}
			if h > float64(s.CellH) {
				scale = math.Min(scale, float64(s.CellH)/h)
			}
		}
	}
	if err := s.setSize(f, f.base*scale); err != nil {
		return empty
	}
	defer s.setSize(f, f.base)

	canvas := newCanvas(availW, s.CellH)
	originX := 0
	for i, r := range text {
		gi := C.FT_Get_Char_Index(f.ft, C.FT_ULong(r))
		if gi == 0 && i > 0 {
			continue // combining mark the font lacks; draw the base only
		}
		emb, obl := C.int(0), C.int(0)
		if f.embolden {
			emb = 1
		}
		if f.oblique {
			obl = 1
		}
		if C.ng_ft_load(f.ft, gi, f.loadFlags, emb, obl) != 0 {
			continue
		}
		mono := C.int(0)
		if f.mono {
			mono = 1
		}
		if C.ng_ft_render(f.ft, mono) != 0 {
			continue
		}
		slot := C.ng_slot(f.ft)
		if i == 0 && (!primary || cells != 1) {
			// Center the advance width inside the cells.
			adv := int(math.Round(float64(slot.advance.x) / 64))
			originX = max(0, (availW-adv)/2)
		}
		// Keep the font's own baseline when it fits; otherwise center the
		// glyph's vertical extent in the cell.
		baseline := s.Baseline
		if !primary {
			asc := ceil26(C.ng_ascender(f.ft))
			desc := -floor26(C.ng_descender(f.ft))
			if asc+desc <= s.CellH {
				baseline = max(asc, min(s.Baseline, s.CellH-desc))
			} else {
				baseline = (s.CellH-(asc+desc))/2 + asc
			}
		}
		canvas.draw(&slot.bitmap, originX+int(slot.bitmap_left), baseline-int(slot.bitmap_top))
	}
	return canvas.trim()
}

// canvas accumulates coverage for one cell group, clipped to its bounds.
type canvas struct {
	w, h int
	pix  []byte
}

func newCanvas(w, h int) *canvas { return &canvas{w: w, h: h, pix: make([]byte, w*h)} }

func (c *canvas) draw(bm *C.FT_Bitmap, x0, y0 int) {
	rows, width := int(bm.rows), int(bm.width)
	if rows == 0 || width == 0 || bm.buffer == nil {
		return
	}
	pitch := int(bm.pitch)
	absPitch := pitch
	if absPitch < 0 {
		absPitch = -absPitch
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(bm.buffer)), absPitch*rows)
	for y := 0; y < rows; y++ {
		cy := y0 + y
		if cy < 0 || cy >= c.h {
			continue
		}
		row := src[y*absPitch:]
		if pitch < 0 {
			row = src[(rows-1-y)*absPitch:]
		}
		for x := 0; x < width; x++ {
			cx := x0 + x
			if cx < 0 || cx >= c.w {
				continue
			}
			var a byte
			switch bm.pixel_mode {
			case C.FT_PIXEL_MODE_MONO:
				if row[x>>3]&(0x80>>(x&7)) != 0 {
					a = 0xff
				}
			case C.FT_PIXEL_MODE_GRAY:
				a = row[x]
			case C.FT_PIXEL_MODE_BGRA:
				a = row[x*4+3]
			default:
				continue
			}
			if p := &c.pix[cy*c.w+cx]; a > *p {
				*p = a
			}
		}
	}
}

// trim crops the canvas to the inked area.
func (c *canvas) trim() *Glyph {
	minX, minY, maxX, maxY := c.w, c.h, -1, -1
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			if c.pix[y*c.w+x] != 0 {
				minX, maxX = min(minX, x), max(maxX, x)
				minY, maxY = min(minY, y), max(maxY, y)
			}
		}
	}
	if maxX < 0 {
		return &Glyph{}
	}
	w, h := maxX-minX+1, maxY-minY+1
	g := &Glyph{X: minX, Y: minY, W: w, H: h, Stride: w, Pix: make([]byte, w*h)}
	for y := 0; y < h; y++ {
		copy(g.Pix[y*w:(y+1)*w], c.pix[(minY+y)*c.w+minX:])
	}
	return g
}
