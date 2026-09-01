package cypher

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/antlr4-go/antlr/v4"
	"github.com/svlocks/sheets/internal/cypher/parsergen"
)

// Parser owns source text. All generated-lexer, generated-parser, and binder
// state is local to Parse, so a Parser may be reused and Parse is safe to call
// concurrently for different values.
type Parser struct{ Source string }

// NewParser creates a parser for source.
func NewParser(source string) *Parser { return &Parser{Source: source} }

// Parse parses the parser's source.
func (p *Parser) Parse() (*Document, error) { return Parse(p.Source) }

// Parse parses a semicolon-delimited Cypher document. Each non-empty host
// statement is parsed by the generated parser derived from the checksum-pinned
// openCypher 9 M23 grammar, then bound directly from its CST to the public AST.
// A failed statement does not prevent later independent statements from being
// parsed, but callers must treat any returned error as making the document
// unsuitable for execution.
func Parse(source string) (*Document, error) {
	document := &Document{Source: source}
	index := newSourceIndex(source)
	segments := splitDocument(source, index)
	var errs Errors
	for _, segment := range segments {
		if segment.empty {
			continue
		}
		tree, parseErrs := parseCST(source, index, segment)
		if len(parseErrs) != 0 {
			for _, parseErr := range parseErrs {
				errs = append(errs, parseErr)
			}
			continue
		}
		statement, err := newBinder(index, segment).bindCypher(tree)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		document.Statements = append(document.Statements, statement)
	}
	if len(errs) == 1 {
		return document, errs[0]
	}
	if len(errs) != 0 {
		return document, errs
	}
	return document, nil
}

// ParseStatements is a convenience wrapper around Parse.
func ParseStatements(source string) ([]Statement, error) {
	document, err := Parse(source)
	if document == nil {
		return nil, err
	}
	return document.Statements, err
}

// MustParse parses source and panics if it is invalid. It is intended only for
// fixed program and test fixtures.
func MustParse(source string) *Document {
	document, err := Parse(source)
	if err != nil {
		panic(err)
	}
	return document
}

type sourceIndex struct {
	byteOffsets []int
	positions   []Position
}

func newSourceIndex(source string) *sourceIndex {
	index := &sourceIndex{
		byteOffsets: []int{0},
		positions:   []Position{{Line: 1, Column: 1}},
	}
	offset, line, column := 0, 1, 1
	for offset < len(source) {
		r, width := utf8.DecodeRuneInString(source[offset:])
		if width == 0 {
			break
		}
		offset += width
		if r == '\n' {
			line++
			column = 1
		} else {
			column++
		}
		index.byteOffsets = append(index.byteOffsets, offset)
		index.positions = append(index.positions, Position{Offset: offset, Line: line, Column: column})
	}
	return index
}

func (i *sourceIndex) runeCount() int { return len(i.byteOffsets) - 1 }

func (i *sourceIndex) position(runeOffset int) Position {
	if runeOffset < 0 {
		runeOffset = 0
	}
	if runeOffset >= len(i.positions) {
		runeOffset = len(i.positions) - 1
	}
	return i.positions[runeOffset]
}

func (i *sourceIndex) byteOffset(runeOffset int) int {
	if runeOffset < 0 {
		return 0
	}
	if runeOffset >= len(i.byteOffsets) {
		return i.byteOffsets[len(i.byteOffsets)-1]
	}
	return i.byteOffsets[runeOffset]
}

type documentSegment struct {
	startRune int
	endRune   int
	empty     bool
}

func (s documentSegment) text(source string, index *sourceIndex) string {
	return source[index.byteOffset(s.startRune):index.byteOffset(s.endRune)]
}

// splitDocument uses the generated lexer so semicolons in quoted strings,
// escaped identifiers, and comments cannot become host statement separators.
// A semicolon token is necessarily outside the official single-query grammar;
// splitting every such token also preserves recovery after an unmatched opener.
func splitDocument(source string, index *sourceIndex) []documentSegment {
	input := antlr.NewInputStream(source)
	lexer := parsergen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	tokens := lexer.GetAllTokens()
	segments := make([]documentSegment, 0, 1)
	start := 0
	for _, token := range tokens {
		if token.GetTokenType() != parsergen.CypherLexerT__0 || token.GetText() != ";" {
			continue
		}
		segments = append(segments, makeSegment(source, index, start, token.GetStart()))
		start = token.GetStop() + 1
	}
	segments = append(segments, makeSegment(source, index, start, index.runeCount()))
	return segments
}

func makeSegment(source string, index *sourceIndex, start, end int) documentSegment {
	segment := documentSegment{startRune: start, endRune: end}
	text := segment.text(source, index)
	mapper := segmentMapper{index: index, segment: segment}
	listener := &syntaxErrorListener{DefaultErrorListener: antlr.NewDefaultErrorListener(), mapper: mapper, source: text}
	lexer := parsergen.NewCypherLexer(antlr.NewInputStream(text))
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(listener)
	segment.empty = true
	for _, token := range lexer.GetAllTokens() {
		if token.GetTokenType() != parsergen.CypherLexerSP {
			segment.empty = false
			break
		}
	}
	// Invalid non-trivia can yield no token at all. Such a segment still has
	// to reach parseCST so the generated lexer error is returned to callers
	// rather than being silently mistaken for an empty statement.
	if len(listener.errors) != 0 {
		segment.empty = false
	}
	return segment
}

func parseCST(source string, index *sourceIndex, segment documentSegment) (tree parsergen.IOC_CypherContext, errs []*ParseError) {
	defer func() {
		if recovered := recover(); recovered != nil {
			position := index.position(segment.startRune)
			errs = append(errs, &ParseError{
				Position: position,
				End:      position,
				Message:  fmt.Sprintf("generated parser stopped safely: %v", recovered),
			})
			tree = nil
		}
	}()

	text := segment.text(source, index)
	mapper := segmentMapper{index: index, segment: segment}
	listener := &syntaxErrorListener{DefaultErrorListener: antlr.NewDefaultErrorListener(), mapper: mapper, source: text}
	lexer := parsergen.NewCypherLexer(antlr.NewInputStream(text))
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(listener)
	tokens := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	parser := parsergen.NewCypherParser(tokens)
	parser.RemoveErrorListeners()
	parser.AddErrorListener(listener)
	parser.BuildParseTrees = true
	tree = parser.OC_Cypher()
	return tree, listener.errors
}

type segmentMapper struct {
	index   *sourceIndex
	segment documentSegment
}

func (m segmentMapper) position(localRune int) Position {
	return m.index.position(m.segment.startRune + localRune)
}

func (m segmentMapper) tokenSpan(token antlr.Token) Span {
	if token == nil {
		position := m.position(0)
		return Span{Start: position, End: position}
	}
	start := token.GetStart()
	if start < 0 {
		start = 0
	}
	end := token.GetStop() + 1
	if token.GetTokenType() == antlr.TokenEOF || end < start {
		end = start
	}
	return Span{Start: m.position(start), End: m.position(end)}
}

func (m segmentMapper) ruleSpan(rule antlr.ParserRuleContext) Span {
	if rule == nil {
		position := m.position(0)
		return Span{Start: position, End: position}
	}
	start := m.tokenSpan(rule.GetStart()).Start
	end := m.tokenSpan(rule.GetStop()).End
	return Span{Start: start, End: end}
}

type syntaxErrorListener struct {
	*antlr.DefaultErrorListener
	mapper segmentMapper
	source string
	errors []*ParseError
}

func (l *syntaxErrorListener) SyntaxError(_ antlr.Recognizer, offendingSymbol interface{}, line, column int, message string, _ antlr.RecognitionException) {
	span := Span{}
	if token, ok := offendingSymbol.(antlr.Token); ok {
		span = l.mapper.tokenSpan(token)
	} else {
		localRune := runeOffsetAtLineColumn(l.source, line, column)
		position := l.mapper.position(localRune)
		span = Span{Start: position, End: position}
	}
	message = normalizeSyntaxMessage(message)
	localOffset := localByteOffset(l.source, span.Start, l.mapper)
	if strings.Contains(strings.ToUpper(l.source[:localOffset]), "RETURN") &&
		(strings.Contains(message, "found") || strings.Contains(message, "near ' ")) &&
		isClauseKeywordFromMessage(message) {
		message = "RETURN must be the final clause"
	}
	l.errors = append(l.errors, &ParseError{Position: span.Start, End: span.End, Message: message})
}

func normalizeSyntaxMessage(message string) string {
	if before, after, ok := strings.Cut(message, " expecting "); ok {
		found := strings.TrimPrefix(before, "mismatched input ")
		found = strings.TrimPrefix(found, "extraneous input ")
		return "expected " + after + " (found " + found + ")"
	}
	if strings.HasPrefix(message, "no viable alternative at input ") {
		return "expected valid Cypher syntax near " + strings.TrimPrefix(message, "no viable alternative at input ")
	}
	return message
}

func isClauseKeywordFromMessage(message string) bool {
	upper := strings.ToUpper(message)
	for _, keyword := range []string{"MATCH", "OPTIONAL", "UNWIND", "WITH", "RETURN", "CREATE", "MERGE", "SET", "REMOVE", "DELETE", "DETACH", "CALL"} {
		if strings.Contains(upper, "'"+keyword+"'") || strings.Contains(upper, " "+keyword) {
			return true
		}
	}
	return false
}

func localByteOffset(source string, position Position, mapper segmentMapper) int {
	globalStart := mapper.index.byteOffset(mapper.segment.startRune)
	offset := position.Offset - globalStart
	if offset < 0 {
		return 0
	}
	if offset > len(source) {
		return len(source)
	}
	return offset
}

func runeOffsetAtLineColumn(source string, targetLine, targetColumn int) int {
	line, column, runeOffset := 1, 0, 0
	for _, r := range source {
		if line == targetLine && column >= targetColumn {
			return runeOffset
		}
		runeOffset++
		if r == '\n' {
			line++
			column = 0
		} else {
			column++
		}
	}
	return runeOffset
}

// String gives a compact debugging representation of a position.
func (p Position) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Column) }
