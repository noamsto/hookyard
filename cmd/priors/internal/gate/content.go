package gate

import "regexp"

// MaxFactBytes is gate 4's per-fact size cap.
const MaxFactBytes = 8192

var contentClasses = []struct {
	class string
	re    *regexp.Regexp
}{
	{"content:url", regexp.MustCompile(`(?i)https?://\S+|\bwww\.[a-z0-9-]+\.[a-z]{2,}`)},
	{"content:pipe-to-shell", regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b[^\n]*\|\s*(sudo\s+(-\S+\s+)*)?(\S*/)?(sh|bash|zsh|fish|dash|python3?)\b`)},
	{"content:hook-bypass", regexp.MustCompile(`(?i)--no-verify|--no-gpg-sign|--dangerously-skip-permissions|core\.hookspath`)},
	{"content:command", regexp.MustCompile("(?im)^[ \\t]*(```|~~~)[ \\t]*(sh|bash|shell|zsh|fish|console)\\b|^[ \\t]*\\$ \\S|\\bsudo\\s+\\S|\\brm\\s+-[a-z]*r[a-z]*f|\\brm\\s+-[a-z]*f[a-z]*r")},
	{"content:always-never", regexp.MustCompile(`(?i)\b(always|never)\s+(run|execute|use|call|invoke|approve|allow|skip|bypass|disable|push|commit|merge|install|delete|trust|ignore|pass|force)\b`)},
	{"content:instruction", regexp.MustCompile(`(?i)ignore\s+(all\s+|any\s+|the\s+)?(previous|prior|above|earlier)\s+(instructions|rules|messages)|\bdisregard\b|\bsystem\s+prompt\b|\bfrom\s+now\s+on\b|\byou\s+(must|should)\s+(now|always|never)\b|\bnew\s+instructions\b|\bdo\s+not\s+(tell|inform|mention|reveal)\b`)},
}

// Content returns the gate-3 classes text trips, each once, in class order.
func Content(text string) []string {
	var classes []string
	for _, c := range contentClasses {
		if c.re.MatchString(text) {
			classes = append(classes, c.class)
		}
	}
	return classes
}

// Size returns gate 4's reason when a fact of n bytes exceeds the cap.
func Size(n int) []string {
	if n > MaxFactBytes {
		return []string{"size"}
	}
	return nil
}
