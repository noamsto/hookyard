package sanitize

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The properties, checked over every code point c (docs/design/memory-layer.md
// §4.4 and §7): E2 inserts each stripped rune anywhere in a protected line; E3
// substitutes c for token letters; E4 inserts c inside a token; E5 wraps the
// headers in every bracket pair. A substitution is exempt when it is not a
// lookalike: R3, c normalises to visible ASCII or maps to another letter; R4,
// c normalises to nothing, a deletion; R7, c normalises to several words.
//
// Strides keep the generator-driven properties inside the CI budget:
// strippable, bracket-like, confusables-table and assigned code points are
// never strided; that exhaustive run is gated by `nix flake check`
// (checks.priors runs the package's tests) and the pre-push gotest hook. Under
// -race the properties that walk the code points (TestPropertyEscapes,
// TestPropertyGluedForeignBenign) are skipped: the detector multiplies the
// cost several times over on a few-core runner and adds nothing for a pure
// function, and the exhaustive run without -race covers them.
const (
	chunkCount      = 32
	unassignedStep  = 97 // Co and unassigned
	maxChunkFailure = 20
)

// protected is one line Text must keep escaped, with the rune positions of the
// letters spelling its token.
type protected struct {
	text    string
	token   string
	fence   bool
	runes   []rune
	off     []int // byte offset of each rune; off[len(runes)] == len(text)
	letters []int
}

func newProtected(text, token string, fence bool) protected {
	p := protected{text: text, token: token, fence: fence, runes: []rune(text)}
	for i := range p.runes {
		p.off = append(p.off, len(string(p.runes[:i])))
	}
	p.off = append(p.off, len(text))
	k := 0
	for i, r := range p.runes {
		if k < len(token) && unicode.ToLower(r) == rune(token[k]) {
			p.letters = append(p.letters, i)
			k++
		}
	}
	if k != len(token) {
		panic("token letters not found in " + text)
	}
	return p
}

func (p protected) replace(i int, s string) string {
	return p.text[:p.off[i]] + s + p.text[p.off[i+1]:]
}

// replaceRange replaces runes i through j-1 with s.
func (p protected) replaceRange(i, j int, s string) string {
	return p.text[:p.off[i]] + s + p.text[p.off[j]:]
}

func (p protected) insert(i int, s string) string {
	return p.text[:p.off[i]] + s + p.text[p.off[i]:]
}

var protectedLines = []protected{
	newProtected("[hookyard advisory]", "hookyardadvisory", false),
	newProtected("[priors memory · work]", "priorsmemory", false),
	newProtected("===== BEGIN priors-0123456789abcdef =====", "beginpriors", true),
	newProtected("===== END priors-0123456789abcdef =====", "endpriors", true),
}

// isolation separators cycle by position: space, thin space, hair space.
var isolators = []string{" ", "\u2009", "\u200a"}

// sideSeps also hold the blank set, which reads as space.
var sideSeps = []string{" ", "\u2009", "\u200a", "\u2800", "\U0001d159"}

// wrapped are the two headers inside non-ASCII brackets and symbols, which
// glue onto a lookalike at either end of the header.
var wrapped = func() []protected {
	var ps []protected
	for _, w := range [][2]string{{"\u228f", "\u2290"}, {"\u23a1", "\u23a6"}, {"\u227a", "\u227b"}, {"\u2605", "\u2605"}, {"\u300c", "\u300d"}, {"\u228f\u228f", "\u2290\u2290"}} {
		ps = append(ps,
			newProtected(w[0]+"hookyard advisory"+w[1], "hookyardadvisory", false),
			newProtected(w[0]+"priors memory · work"+w[1], "priorsmemory", false))
	}
	return ps
}()

func isolate(c rune, pos int) string {
	sep := isolators[pos%len(isolators)]
	return sep + string(c) + sep
}

// strippable is the stripped set, written from the design rather than from
// sanitize.go.
func strippable(c rune) bool {
	if c == '\n' || c == '\t' {
		return false
	}
	return unicode.In(c, unicode.Cc, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}

func assigned(c rune) bool {
	return unicode.In(c, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf)
}

// bracketLike is the E5 set minus the ASCII brackets.
func bracketLike(c rune) bool {
	switch {
	case unicode.In(c, unicode.Ps, unicode.Pe, unicode.Pi, unicode.Pf):
		return true
	case 0x239b <= c && c <= 0x23b3, 0x2308 <= c && c <= 0x230b, 0x231c <= c && c <= 0x231f,
		c == 0x2282, c == 0x2283, 0x228f <= c && c <= 0x2292, c == 0x22d0, c == 0x22d1,
		c == 0x23b4, c == 0x23b5, 0x23dc <= c && c <= 0x23e1, c == 0x227a, c == 0x227b,
		c == 0x22b0, c == 0x22b1, c == 0x2af7, c == 0x2af8:
		return true
	}
	return false
}

// image is what c normalises to, rune by rune, as the skeleton would see it.
type image struct {
	sep     bool
	literal bool
	equals  bool
	runes   int
	anchors []uint64 // confusable-set masks of the anchor-image runes
}

func (im image) clean() bool { return !im.sep && !im.literal && !im.equals }

func imageOf(c rune) image {
	var im image
	if strippable(c) {
		return im // removed before NFKC: a deletion (R4)
	}
	n := norm.NFKC.String(string(c))
	for _, r := range n {
		if unicode.IsSpace(r) || unicode.Is(unicode.Z, r) || oracleBlank(r) {
			im.sep = true
		}
	}
	for _, r := range n {
		if unicode.Is(unicode.M, r) {
			continue
		}
		base := []rune(norm.NFD.String(string(r)))[0]
		if unicode.Is(unicode.M, base) {
			continue
		}
		lb := unicode.ToLower(base)
		im.runes++
		var set uint64
		if lb < 0x80 {
			if imageBit(lb) == 0 {
				im.literal = true
				continue
			}
			set = imageBit(lb)
		} else {
			set = confusableSets[base] | confusableSets[lb]
		}
		switch {
		case set == equalsBit:
			im.equals = true
		case set&(equalsBit-1) != 0:
			im.anchors = append(im.anchors, set)
		}
	}
	return im
}

type counts struct {
	e3R3, e3R4, e3R7, e3Multi, e3NoLetter atomic.Int64
	e3Shaped                              atomic.Int64
	e4R3, e4R7, e4Anchor                  atomic.Int64
	tested, e2, e3, e4                    atomic.Int64
	cases                                 atomic.Int64
}

type checker struct {
	t      *testing.T
	failed int
	cases  int64
}

// check runs Text on in and applies the oracle: every line is quoted whole,
// or a token is quoted and the line's own is escaped. A non-empty needle is
// c's image, placed inside the token: outside quoted spans it may occur only
// as often as in the protected line itself (the store header's own '·').
func (ck *checker) check(c rune, kind string, p protected, in, needle string) {
	ck.cases++
	out := Text(in)
	if linesQuoted(out) {
		return
	}
	if quotesAToken(out) && escaped(out, p.token) && (!p.fence || !ruleRunLeft(out)) &&
		(needle == "" || strings.Count(unquoted(out), needle) <= strings.Count(p.text, needle)) {
		return
	}
	ck.t.Errorf("%s U+%04X token %s: Text(%q) = %q", kind, c, p.token, in, out)
	if ck.failed++; ck.failed >= maxChunkFailure {
		ck.t.Fatalf("stopping chunk after %d failures", ck.failed)
	}
}

func linesQuoted(out string) bool {
	for l := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(l, "(quoted line: ") {
			return false
		}
	}
	return true
}

// quotesAToken reports a quoted protected token. It need not be the line's
// own: an earlier match, such as glued symbols before "priors" reading as
// "endpriors" on a fence-context line, can take letters the line's own token
// needed while still leaving no header behind.
func quotesAToken(out string) bool {
	for _, t := range tokens {
		if strings.Contains(out, "(quoted: "+t.s+")") {
			return true
		}
	}
	return false
}

// needleOf is NFKC(c) when it holds a non-ASCII rune, else empty: an ASCII
// image is indistinguishable from the line's own letters.
func needleOf(c rune) string {
	n := norm.NFKC.String(string(c))
	for _, r := range n {
		if r >= 0x80 {
			return n
		}
	}
	return ""
}

type codePoint struct {
	c     rune
	strip bool
}

func testedCodePoints() (cps []codePoint, desc string) {
	all := os.Getenv("SANITIZE_ALL_CODEPOINTS") == "1"
	var seq int
	exempt, strided := 0, 0
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if 0xd800 <= c && c <= 0xdfff {
			continue
		}
		strip := strippable(c)
		if c < 0x80 && !strip {
			continue
		}
		_, inTable := confusableSets[c]
		switch {
		case all:
			exempt++
		case strip || bracketLike(c) || inTable:
			exempt++
		case !assigned(c):
			if seq++; (seq-1)%unassignedStep != 0 {
				continue
			}
			strided++
		default:
			exempt++
		}
		cps = append(cps, codePoint{c, strip})
	}
	desc = fmt.Sprintf("all=%v unstrided=%d strided=%d (unassigned/Co step %d)",
		all, exempt, strided, unassignedStep)
	return cps, desc
}

func TestPropertyEscapes(t *testing.T) {
	if testing.Short() {
		t.Skip("generator-driven property test")
	}
	if raceEnabled {
		t.Skip("pure function: the exhaustive run without -race covers it")
	}
	cps, desc := testedCodePoints()
	t.Logf("code points: %d, %s", len(cps), desc)
	var n counts
	t.Run("chunks", func(t *testing.T) {
		for i := range chunkCount {
			t.Run(fmt.Sprintf("chunk%02d", i), func(t *testing.T) {
				t.Parallel()
				ck := &checker{t: t}
				for j := i; j < len(cps); j += chunkCount {
					propertiesOf(ck, cps[j], &n)
				}
				n.cases.Add(ck.cases)
			})
		}
	})
	t.Logf("tested code points %d (E2 %d, E3 %d, E4 %d); Text calls %d", n.tested.Load(), n.e2.Load(), n.e3.Load(), n.e4.Load(), n.cases.Load())
	t.Logf("E3 pair, one-sided and blank-companion variants: %d code points", n.e3Shaped.Load())
	t.Logf("E3 excluded: R3 literal/equals %d, R3 anchor for no token letter %d, R4 empty image %d, R7 separator %d, more than one anchor %d",
		n.e3R3.Load(), n.e3NoLetter.Load(), n.e3R4.Load(), n.e3R7.Load(), n.e3Multi.Load())
	t.Logf("E4 excluded: R3 literal/equals %d, R7 separator %d, anchor-bearing (E3 only) %d",
		n.e4R3.Load(), n.e4R7.Load(), n.e4Anchor.Load())
}

func propertiesOf(ck *checker, cp codePoint, n *counts) {
	c := cp.c
	n.tested.Add(1)
	if cp.strip {
		n.e2.Add(1)
		s := string(c)
		for _, p := range protectedLines {
			for i := range len(p.runes) + 1 {
				ck.check(c, "E2", p, p.insert(i, s), "")
			}
		}
	}
	if c < 0x80 {
		return
	}
	im := imageOf(c)
	s := string(c)
	isolated := im.runes == 1
	needle := needleOf(c)

	// E3: single substitution.
	switch {
	case im.sep:
		n.e3R7.Add(1)
	case !im.clean():
		n.e3R3.Add(1)
	case im.runes == 0:
		n.e3R4.Add(1)
	case len(im.anchors) > 1:
		n.e3Multi.Add(1)
	default:
		applied := false
		for _, p := range protectedLines {
			for k, i := range p.letters {
				if len(im.anchors) == 1 && im.anchors[0]&imageBit(rune(p.token[k])) == 0 {
					continue
				}
				applied = true
				ck.check(c, "E3 glued", p, p.replace(i, s), needle)
				if isolated {
					ck.check(c, "E3 isolated", p, p.replace(i, isolate(c, i)), needle)
				}
			}
		}
		if applied {
			n.e3.Add(1)
		} else {
			n.e3NoLetter.Add(1)
		}
		if isolated && len(im.anchors) == 0 {
			n.e3Shaped.Add(1)
			shapedSubstitutions(ck, c, needle)
		}
	}

	// E4: single insertion.
	switch {
	case im.sep:
		n.e4R7.Add(1)
	case !im.clean():
		n.e4R3.Add(1)
	case len(im.anchors) > 0:
		n.e4Anchor.Add(1)
	default:
		n.e4.Add(1)
		for _, p := range protectedLines {
			first, last := p.letters[0], p.letters[len(p.letters)-1]
			for i := max(first-1, 0); i <= min(last+2, len(p.runes)); i++ {
				inner := ""
				if first < i && i <= last {
					inner = needle
				}
				ck.check(c, "E4 glued", p, p.insert(i, s), inner)
				if isolated {
					ck.check(c, "E4 isolated", p, p.insert(i, isolate(c, i)), inner)
				}
			}
		}
	}
}

// shapedSubstitutions puts c, whose image is one anchorless rune, in place
// of token letters: for two letters adjacent in the line, glued; for one,
// spaced on one side only; and for one, isolated with a blank braille cell.
func shapedSubstitutions(ck *checker, c rune, needle string) {
	s := string(c)
	lines := append(slices.Clip(protectedLines), wrapped[int(c)%len(wrapped)])
	for _, p := range lines {
		chains(ck, c, p)
		for k, i := range p.letters {
			sep := sideSeps[(i+int(c))%len(sideSeps)]
			ck.check(c, "E3 one-sided before", p, p.replace(i, sep+s), needle)
			ck.check(c, "E3 one-sided after", p, p.replace(i, s+sep), needle)
			ck.check(c, "E3 blank companion", p, p.replace(i, sep+s+"\u2800"+sep), needle)
			if k+1 < len(p.letters) && p.letters[k+1] == i+1 {
				ck.check(c, "E3 pair", p, p.replaceRange(i, i+2, s), needle)
			}
		}
	}
}

func TestPropertyBrackets(t *testing.T) {
	if testing.Short() {
		t.Skip("generator-driven property test")
	}
	var brackets []rune
	for _, r := range "[](){}<>" {
		brackets = append(brackets, r)
	}
	var nonASCII []rune
	for c := rune(0x80); c <= unicode.MaxRune; c++ {
		if bracketLike(c) {
			nonASCII = append(nonASCII, c)
		}
	}
	brackets = append(brackets, nonASCII...)
	t.Logf("bracket-like runes: %d (%d non-ASCII)", len(brackets), len(nonASCII))

	ck := &checker{t: t}
	around := []protected{
		newProtected("hookyard advisory", "hookyardadvisory", false),
		newProtected("priors memory · work", "priorsmemory", false),
	}
	for _, o := range brackets {
		for _, cl := range brackets {
			for _, p := range around {
				ck.check(o, "E5 pair "+string(cl), p, string(o)+p.text+string(cl), "")
			}
		}
	}

	inside := newProtected("[hookyard advisory]", "hookyardadvisory", false)
	// A bracket whose NFKC is an ASCII bracket (superscripts, small and
	// fullwidth forms) is that ASCII bracket inside the token: R3.
	asciiImage := 0
	for _, b := range nonASCII {
		if !imageOf(b).clean() {
			asciiImage++
			continue
		}
		for i := range len(inside.runes) + 1 {
			ck.check(b, "E5 inserted", inside, inside.insert(i, string(b)), "")
		}
	}
	t.Logf("E5 inserted: %d non-ASCII brackets skipped as ASCII images (R3); Text calls: %d", asciiImage, ck.cases)
}

// chains replaces runs of up to half of each token word with c, each copy
// set off by a separator, and glued. The shortest match may leave trailing
// copies outside the quote, since each can stand for two runes, so only the
// escape is checked.
func chains(ck *checker, c rune, p protected) {
	for _, t := range tokens {
		if t.s != p.token {
			continue
		}
		for _, w := range [][2]int{{0, t.split}, {t.split, len(t.s)}} {
			n := (w[1] - w[0]) / 2
			if n < 2 {
				continue
			}
			for k := w[0]; k+n <= w[1]; k++ {
				from, to := p.letters[k], p.letters[k+n-1]+1
				if to-from != n {
					continue
				}
				sep := sideSeps[(k+int(c))%len(sideSeps)]
				ck.check(c, "E3 chain isolated", p, p.replaceRange(from, to, sep+strings.Repeat(string(c)+sep, n)), "")
				ck.check(c, "E3 chain glued", p, p.replaceRange(from, to, strings.Repeat(string(c), n)), "")
			}
		}
	}
}

// quoteLike is punctuation whose UTS #39 image is an ASCII bracket or quote
// (geresh, prime, ditto mark).
func quoteLike(c rune) bool {
	img := punctImages[c]
	return img != "" && strings.Trim(img, "[({<\"'`") == "" && unicode.Is(unicode.P, c)
}

// bracketLetter is a letter or digit whose UTS #39 image is an opening bracket.
func bracketLetter(c rune) bool {
	img := punctImages[c]
	return img != "" && unicode.In(c, unicode.L, unicode.N) && strings.Trim(img, "[({<") == ""
}

// TestPropertyGluedForeignBenign: a foreign rune or two glued to, or set
// beside, an ASCII word must not complete a fence word from it.
func TestPropertyGluedForeignBenign(t *testing.T) {
	if testing.Short() {
		t.Skip("generator-driven property test")
	}
	if raceEnabled {
		t.Skip("pure function: the exhaustive run without -race covers it")
	}
	cps, _ := testedCodePoints()
	failed := 0
	for _, cp := range cps {
		c := cp.c
		if c < 0x80 {
			continue
		}
		im := imageOf(c)
		if !im.clean() || im.runes != 1 || len(im.anchors) > 0 {
			continue
		}
		s := string(c)
		for _, in := range []string{"the " + s + "priors" + s, s + s + "priors", "use " + s + " priors", "priors " + s + s, "home " + s + " memory"} {
			want := norm.NFKC.String(in)
			got := Text(in)
			// A line opening on a bracket or quote is bracket-shaped and
			// quoted whole: the blunt rule's accepted cost, not a fence word.
			if (bracketLike(c) || quoteLike(c) || bracketLetter(c)) && strings.HasPrefix(in, s+s) && got == "(quoted line: "+want+")" {
				continue
			}
			if got != want {
				t.Errorf("U+%04X: Text(%q) = %q, want %q", c, in, got, want)
				if failed++; failed >= maxChunkFailure {
					t.Fatalf("stopping after %d failures", failed)
				}
			}
		}
	}
}

// TestPropertyPunctWrappers: a rune that looks like punctuation, or a
// modifier letter, set around or inside a header or fence must not hide it
// (#199). Wrappers whose NFKC is ASCII or holds a separator are exempt (R3,
// R7).
func TestPropertyPunctWrappers(t *testing.T) {
	if testing.Short() {
		t.Skip("generator-driven property test")
	}
	var xs []rune
	for r := range punctImages {
		xs = append(xs, r)
	}
	for c := rune(0x80); c <= unicode.MaxRune; c++ {
		if unicode.Is(unicode.Lm, c) {
			xs = append(xs, c)
		}
	}
	xs = append(xs, 0x3108, 0x30b3, 0x30fc, 0x318d)
	slices.Sort(xs)
	xs = slices.Compact(xs)

	forms := []struct {
		token string
		fence bool
		of    func(x string) string
	}{
		{"hookyardadvisory", false, func(x string) string { return x + "\u0127 ookyard advisory" + x }},
		{"priorsmemory", false, func(x string) string { return x + "\u01a5 riors memory \u00b7 work" + x }},
		{"hookyardadvisory", false, func(x string) string { return "[hookyard advis \ua74b" + x + " ry]" }},
		{"endpriors", true, func(x string) string { return "===== " + x + "\u0190 \u004eD priors-0123456789abcdef =====" }},
	}
	ck := &checker{t: t}
	skipped := 0
	for _, x := range xs {
		if im := imageOf(x); im.sep || needleOf(x) == "" {
			skipped++
			continue
		}
		for _, f := range forms {
			in := f.of(string(x))
			ck.check(x, "punct wrapper", protected{text: in, token: f.token, fence: f.fence}, in, "")
		}
	}
	t.Logf("wrappers: %d, skipped as ASCII or separator images (R3, R7): %d; Text calls: %d", len(xs), skipped, ck.cases)
}
