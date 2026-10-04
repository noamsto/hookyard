package sanitize

import "testing"

func TestConfusablesTable(t *testing.T) {
	const allBits = 1<<37 - 1
	var exactlyM bool
	for r, mask := range confusableSets {
		if r < 0x80 {
			t.Errorf("U+%04X: ASCII key", r)
		}
		if mask == 0 || mask&^allBits != 0 {
			t.Errorf("U+%04X: mask %#x is empty or has bits above 36", r, mask)
		}
		if mask == imageBit('m') {
			exactlyM = true
		}
	}
	if !exactlyM {
		t.Error("no entry maps to exactly 'm'")
	}

	has := func(r rune, c rune) {
		t.Helper()
		if confusableSets[r]&imageBit(c) == 0 {
			t.Errorf("U+%04X lacks %q (mask %#x)", r, c, confusableSets[r])
		}
	}
	has(0xa4e7, 'h')
	has(0x043e, 'o')
	has(0x041e, 'o')
	has(0xa4f3, 'o')
	has(0x041e, '0')
	has(0x0430, 'a')

	if got, want := confusableSets[0x0399], imageBit('i')|imageBit('l')|imageBit('1'); got != want {
		t.Errorf("U+0399 = %#x, want %#x", got, want)
	}
	if got := confusableSets[0xa60c]; got != equalsBit {
		t.Errorf("U+A60C = %#x, want %#x", got, uint64(equalsBit))
	}
}
