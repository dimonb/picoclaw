package telegram

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	richAmount      = regexp.MustCompile(`^[0-9]+(?:[.,][0-9]+)*(?:[ \x{00a0}\x{202f}][0-9]{3})*(?:[.,][0-9]+)?`)
	richMathCommand = regexp.MustCompile(`\\[a-zA-Z]+`)
	richMathWord    = regexp.MustCompile(`\p{L}\p{M}*\p{L}`)
)

// escapeRichCurrency prevents prose such as "$10 and $20" from becoming one
// server-side math span. Only numeric dollar prefixes outside code/links/math
// are escaped. Closed numeric expressions ($2$, $2+2$) win over currency;
// ambiguous closed math must remain author-controlled. This is deliberately a
// conservative lexer, not a replacement for Telegram's Markdown parser.
func escapeRichCurrency(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '\\' && i+1 < len(text) {
			out.WriteString(text[i : i+2])
			i += 2
			continue
		}
		if end := richLiteralEnd(text, i); end > i {
			out.WriteString(text[i:end])
			i = end
			continue
		}
		if text[i] == '$' {
			if strings.HasPrefix(text[i:], "$$") {
				end := len(text)
				if closing := strings.Index(text[i+2:], "$$"); closing >= 0 {
					end = i + 2 + closing + 2
				}
				out.WriteString(text[i:end])
				i = end
				continue
			}
			if end := richInlineMathEnd(text, i); end > i {
				out.WriteString(text[i:end])
				i = end
				continue
			}
			if amount := richAmount.FindString(text[i+1:]); amount != "" {
				end := i + 1 + len(amount)
				// An unfinished formula or identifier ($2x, $2+...) is not clearly money.
				next, _ := utf8.DecodeRuneInString(text[end:])
				closingEmphasis := richClosingEmphasis(text, end)
				if closingEmphasis || end == len(text) ||
					(end+1 < len(text) && strings.ContainsRune("-/", rune(text[end])) && text[end+1] == '$') ||
					(!unicode.IsLetter(next) && !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_+-*/^=<>\\", next)) {
					out.WriteByte('\\')
				}
			}
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

func richInlineMathEnd(text string, start int) int {
	for i := start + 1; i < len(text); i++ {
		if text[i] == '\n' {
			return start
		}
		if text[i] == '\\' {
			i++
			continue
		}
		if text[i] != '$' {
			continue
		}
		// A dollar immediately followed by a number starts the next price,
		// not the end of math: "$10, $20" and "$10-$20".
		if i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9' {
			return start
		}
		raw := text[start+1 : i]
		// A whitespace-ended price/prose candidate before a new formula is not
		// closed by that formula's opening dollar ("$10 и $x^2$").
		if i+1 < len(text) && richAmount.MatchString(raw) && strings.TrimRight(raw, " \t") != raw {
			next, _ := utf8.DecodeRuneInString(text[i+1:])
			if unicode.IsLetter(next) || next == '\\' {
				return start
			}
		}
		if richMathCommand.MatchString(raw) {
			return i + 1
		}
		body := raw
		if strings.TrimSpace(body) == "" || richMathWord.MatchString(body) {
			return start
		}
		for _, r := range body {
			if !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.Is(unicode.Sm, r) &&
				!strings.ContainsRune(
					"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ \t.,+-*/^_=<>!()[]{}|:;'%&×÷−±≤≥≠∞π",
					r,
				) {
				return start
			}
		}
		return i + 1
	}
	return start
}

// richLiteralEnd protects backtick spans (including multiline spans), fenced
// blocks, Markdown links/images/reference definitions, and bare/autolink URLs.
func richLiteralEnd(text string, start int) int {
	rest := text[start:]
	if start == 0 || text[start-1] == '\n' {
		// Leave indented code untouched, even if the surrounding Markdown is
		// incomplete (as it commonly is in streaming drafts).
		if strings.HasPrefix(rest, "    ") || strings.HasPrefix(rest, "\t") {
			if end := strings.IndexByte(rest, '\n'); end >= 0 {
				return start + end + 1
			}
			return len(text)
		}
	}
	if strings.HasPrefix(rest, "https://") || strings.HasPrefix(rest, "http://") ||
		strings.HasPrefix(rest, "<https://") ||
		strings.HasPrefix(rest, "<http://") {
		if text[start] == '<' {
			if closing := strings.IndexByte(rest, '>'); closing >= 0 {
				return start + closing + 1
			}
		}
		end := start
		for end < len(text) && !strings.ContainsRune(" \t\r\n", rune(text[end])) {
			end++
		}
		return end
	}
	if text[start] == '[' {
		closing := richBalancedEnd(text, start, '[', ']')
		if closing > start && closing < len(text) {
			switch text[closing] {
			case '(':
				if end := richBalancedEnd(text, closing, '(', ')'); end > closing {
					return end
				}
			case '[':
				if end := richBalancedEnd(text, closing, '[', ']'); end > closing {
					return end
				}
			case ':':
				if end := strings.IndexByte(text[closing:], '\n'); end >= 0 {
					return closing + end
				}
				return len(text)
			}
		}
		// Protect shortcut references only when a matching definition exists.
		if closing > start {
			label := strings.Join(strings.Fields(text[start+1:closing-1]), " ")
			definition := regexp.MustCompile(`(?im)^ {0,3}\[` + regexp.QuoteMeta(label) + `\]:[ \t]*\S`)
			if definition.MatchString(text) {
				return closing
			}
		}
	}
	if text[start] != '`' && text[start] != '~' {
		return start
	}
	marker := text[start]
	n := 1
	for start+n < len(text) && text[start+n] == marker {
		n++
	}
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	fenced := n >= 3 && strings.Trim(text[lineStart:start], " >") == ""
	if fenced {
		// A closing fence must be on its own line and at least as long as its opener.
		for pos := start + n; pos < len(text); {
			newline := strings.IndexByte(text[pos:], '\n')
			if newline < 0 {
				break
			}
			pos += newline + 1
			end := strings.IndexByte(text[pos:], '\n')
			if end < 0 {
				end = len(text) - pos
			}
			line := strings.Trim(text[pos:pos+end], " >\t\r")
			if len(line) >= n && strings.Trim(line, string(marker)) == "" {
				return pos + end
			}
		}
		return len(text) // An incomplete streamed fence is still code.
	}
	if marker != '`' {
		return start
	}
	for pos := start + n; pos < len(text); {
		offset := strings.IndexByte(text[pos:], '`')
		if offset < 0 {
			break
		}
		pos += offset
		end := pos
		for end < len(text) && text[end] == '`' {
			end++
		}
		if end-pos == n {
			return end
		}
		pos = end
	}
	return start
}

func richBalancedEnd(text string, start int, open, closing byte) int {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		if text[i] == open {
			depth++
		}
		if text[i] == closing {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return start
}

// Recognize emphasis only at a prose boundary, not in unfinished $2**3 math.
func richClosingEmphasis(text string, start int) bool {
	end := start
	for end < len(text) && (text[end] == '*' || text[end] == '_') {
		end++
	}
	return end > start && (end == len(text) || strings.ContainsRune(" \t\r\n.,;:!?)]}>", rune(text[end])))
}
