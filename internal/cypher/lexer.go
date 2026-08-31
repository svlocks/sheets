package cypher

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenIdentifier
	tokenQuotedIdentifier
	tokenParameter
	tokenInteger
	tokenFloat
	tokenString
	tokenSymbol
	tokenInvalid
)

type token struct {
	kind tokenKind
	text string // decoded spelling for strings/quoted names, raw spelling otherwise
	span Span
}

func (t token) isSymbol(symbol string) bool { return t.kind == tokenSymbol && t.text == symbol }

type lexer struct {
	source string
	offset int
	line   int
	column int
	errors Errors
}

func lex(source string) ([]token, Errors) {
	l := lexer{source: source, line: 1, column: 1}
	tokens := make([]token, 0, len(source)/4+1)
	for {
		l.skipSpaceAndComments()
		start := l.position()
		if l.offset >= len(l.source) {
			tokens = append(tokens, token{kind: tokenEOF, span: Span{Start: start, End: start}})
			break
		}
		ch, _ := l.peekRune()
		switch {
		case isIdentifierStart(ch):
			tokens = append(tokens, l.scanIdentifier(start))
		case ch == '`':
			tokens = append(tokens, l.scanQuotedIdentifier(start))
		case ch == '$':
			tokens = append(tokens, l.scanParameter(start))
		case ch == '\'' || ch == '"':
			tokens = append(tokens, l.scanString(start, ch))
		case unicode.IsDigit(ch) || (ch == '.' && l.peekSecondIsDigit()):
			tokens = append(tokens, l.scanNumber(start))
		default:
			tokens = append(tokens, l.scanSymbol(start))
		}
	}
	return tokens, l.errors
}

func (l *lexer) position() Position {
	return Position{Offset: l.offset, Line: l.line, Column: l.column}
}

func (l *lexer) peekRune() (rune, int) {
	if l.offset >= len(l.source) {
		return 0, 0
	}
	return utf8.DecodeRuneInString(l.source[l.offset:])
}

func (l *lexer) peekN(n int) byte {
	i := l.offset + n
	if i >= len(l.source) {
		return 0
	}
	return l.source[i]
}

func (l *lexer) advanceRune() rune {
	r, n := l.peekRune()
	if n == 0 {
		return 0
	}
	l.offset += n
	if r == '\n' {
		l.line++
		l.column = 1
	} else {
		l.column++
	}
	return r
}

func (l *lexer) skipSpaceAndComments() {
	for {
		for {
			r, n := l.peekRune()
			if n == 0 || !unicode.IsSpace(r) {
				break
			}
			l.advanceRune()
		}
		if l.peekN(0) == '/' && l.peekN(1) == '/' {
			l.advanceRune()
			l.advanceRune()
			for {
				r, n := l.peekRune()
				if n == 0 || r == '\n' || r == '\r' {
					break
				}
				l.advanceRune()
			}
			continue
		}
		if l.peekN(0) == '/' && l.peekN(1) == '*' {
			start := l.position()
			l.advanceRune()
			l.advanceRune()
			for l.offset < len(l.source) && !(l.peekN(0) == '*' && l.peekN(1) == '/') {
				l.advanceRune()
			}
			if l.offset >= len(l.source) {
				l.errors = append(l.errors, &ParseError{Position: start, End: l.position(), Message: "unterminated block comment"})
				return
			}
			l.advanceRune()
			l.advanceRune()
			continue
		}
		return
	}
}

func isIdentifierStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentifierPart(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func (l *lexer) scanIdentifier(start Position) token {
	from := l.offset
	l.advanceRune()
	for {
		r, n := l.peekRune()
		if n == 0 || !isIdentifierPart(r) {
			break
		}
		l.advanceRune()
	}
	return token{kind: tokenIdentifier, text: l.source[from:l.offset], span: Span{Start: start, End: l.position()}}
}

func (l *lexer) scanQuotedIdentifier(start Position) token {
	l.advanceRune() // opening backtick
	var value strings.Builder
	terminated := false
	for l.offset < len(l.source) {
		if l.peekN(0) == '`' {
			l.advanceRune()
			if l.peekN(0) == '`' { // Cypher escapes a backtick by doubling it.
				value.WriteByte('`')
				l.advanceRune()
				continue
			}
			terminated = true
			break
		}
		r, _ := l.peekRune()
		value.WriteRune(r)
		l.advanceRune()
	}
	span := Span{Start: start, End: l.position()}
	if !terminated {
		l.errors = append(l.errors, &ParseError{Position: start, End: span.End, Message: "unterminated quoted identifier"})
		return token{kind: tokenInvalid, text: value.String(), span: span}
	}
	return token{kind: tokenQuotedIdentifier, text: value.String(), span: span}
}

func (l *lexer) scanParameter(start Position) token {
	l.advanceRune()
	from := l.offset
	r, n := l.peekRune()
	if n == 0 || !isIdentifierStart(r) {
		span := Span{Start: start, End: l.position()}
		l.errors = append(l.errors, &ParseError{Position: start, End: span.End, Message: "expected parameter name after '$'"})
		return token{kind: tokenInvalid, text: "$", span: span}
	}
	l.advanceRune()
	for {
		r, n = l.peekRune()
		if n == 0 || !isIdentifierPart(r) {
			break
		}
		l.advanceRune()
	}
	return token{kind: tokenParameter, text: l.source[from:l.offset], span: Span{Start: start, End: l.position()}}
}

func (l *lexer) scanString(start Position, quote rune) token {
	l.advanceRune()
	var value strings.Builder
	terminated := false
	for l.offset < len(l.source) {
		r, _ := l.peekRune()
		if r == quote {
			l.advanceRune()
			terminated = true
			break
		}
		if r == '\\' {
			l.advanceRune()
			escaped, n := l.peekRune()
			if n == 0 {
				break
			}
			switch escaped {
			case '\'', '"', '\\', '/':
				value.WriteRune(escaped)
				l.advanceRune()
			case 'b':
				value.WriteByte('\b')
				l.advanceRune()
			case 'f':
				value.WriteByte('\f')
				l.advanceRune()
			case 'n':
				value.WriteByte('\n')
				l.advanceRune()
			case 'r':
				value.WriteByte('\r')
				l.advanceRune()
			case 't':
				value.WriteByte('\t')
				l.advanceRune()
			case 'u':
				l.advanceRune()
				var runes [4]rune
				valid := true
				for i := range runes {
					hex, hexN := l.peekRune()
					if hexN == 0 || !isHex(hex) {
						valid = false
						break
					}
					runes[i] = hex
					l.advanceRune()
				}
				if !valid {
					span := Span{Start: start, End: l.position()}
					l.errors = append(l.errors, &ParseError{Position: span.End, End: span.End, Message: "invalid unicode escape"})
					continue
				}
				var code rune
				for _, hex := range runes {
					code = code*16 + hexValue(hex)
				}
				value.WriteRune(code)
			default:
				span := Span{Start: start, End: l.position()}
				l.errors = append(l.errors, &ParseError{Position: span.End, End: span.End, Message: "invalid string escape"})
				value.WriteRune(escaped)
				l.advanceRune()
			}
			continue
		}
		value.WriteRune(r)
		l.advanceRune()
	}
	span := Span{Start: start, End: l.position()}
	if !terminated {
		l.errors = append(l.errors, &ParseError{Position: start, End: span.End, Message: "unterminated string literal"})
		return token{kind: tokenInvalid, text: value.String(), span: span}
	}
	return token{kind: tokenString, text: value.String(), span: span}
}

func isHex(r rune) bool {
	return ('0' <= r && r <= '9') || ('a' <= r && r <= 'f') || ('A' <= r && r <= 'F')
}
func hexValue(r rune) rune {
	switch {
	case r >= '0' && r <= '9':
		return r - '0'
	case r >= 'a' && r <= 'f':
		return r - 'a' + 10
	default:
		return r - 'A' + 10
	}
}

func (l *lexer) peekSecondIsDigit() bool {
	if l.offset+1 >= len(l.source) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(l.source[l.offset+1:])
	return unicode.IsDigit(r)
}

func (l *lexer) scanNumber(start Position) token {
	from := l.offset
	if l.peekN(0) == '.' {
		l.advanceRune()
	}
	for unicode.IsDigit(mustPeekRune(l)) {
		l.advanceRune()
	}
	isFloat := false
	if l.peekN(0) == '.' && l.peekN(1) != '.' {
		isFloat = true
		l.advanceRune()
		for unicode.IsDigit(mustPeekRune(l)) {
			l.advanceRune()
		}
	}
	if l.peekN(0) == 'e' || l.peekN(0) == 'E' {
		isFloat = true
		l.advanceRune()
		if l.peekN(0) == '+' || l.peekN(0) == '-' {
			l.advanceRune()
		}
		digits := 0
		for unicode.IsDigit(mustPeekRune(l)) {
			digits++
			l.advanceRune()
		}
		if digits == 0 {
			span := Span{Start: start, End: l.position()}
			l.errors = append(l.errors, &ParseError{Position: span.End, End: span.End, Message: "invalid exponent in number literal"})
		}
	}
	kind := tokenInteger
	if isFloat {
		kind = tokenFloat
	}
	return token{kind: kind, text: l.source[from:l.offset], span: Span{Start: start, End: l.position()}}
}

func mustPeekRune(l *lexer) rune {
	r, _ := l.peekRune()
	return r
}

func (l *lexer) scanSymbol(start Position) token {
	for _, symbol := range []string{"->", "<-", "<=", ">=", "<>", "!=", "=~", "+=", ".."} {
		if strings.HasPrefix(l.source[l.offset:], symbol) {
			for range symbol {
				l.advanceRune()
			}
			return token{kind: tokenSymbol, text: symbol, span: Span{Start: start, End: l.position()}}
		}
	}
	r := l.advanceRune()
	if strings.ContainsRune("()[]{},;:.|+-*/%^=<>!", r) {
		return token{kind: tokenSymbol, text: string(r), span: Span{Start: start, End: l.position()}}
	}
	span := Span{Start: start, End: l.position()}
	l.errors = append(l.errors, &ParseError{Position: start, End: span.End, Message: "unexpected character " + string(r)})
	return token{kind: tokenInvalid, text: string(r), span: span}
}
