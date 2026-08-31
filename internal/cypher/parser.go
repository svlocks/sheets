package cypher

import (
	"fmt"
	"strconv"
	"strings"
)

// Parser owns source text. It is safe to call Parse on it repeatedly because
// all token and syntax state is local to each call.
type Parser struct{ Source string }

// NewParser creates a parser for source.
func NewParser(source string) *Parser { return &Parser{Source: source} }

// Parse parses the parser's source.
func (p *Parser) Parse() (*Document, error) { return Parse(p.Source) }

// Parse parses a semicolon-separated OpenCypher document. The returned
// document contains all statements successfully parsed before and after a
// recoverable statement error. If parsing fails, err is either *ParseError or
// Errors and every position is relative to source.
func Parse(source string) (*Document, error) {
	tokens, lexErrors := lex(source)
	p := syntaxParser{tokens: tokens}
	doc := &Document{Source: source}
	errs := append(Errors(nil), lexErrors...)

	for !p.atEOF() {
		if p.matchSymbol(";") {
			continue
		}
		statement, err := p.parseStatement(false)
		if err != nil {
			errs = append(errs, asParseError(err, p.current().span))
			p.recoverStatement()
			continue
		}
		doc.Statements = append(doc.Statements, statement)
		if p.matchSymbol(";") {
			continue
		}
		if !p.atEOF() {
			errs = append(errs, p.errorAtCurrent("expected ';' between statements"))
			p.recoverStatement()
		}
	}
	if len(errs) > 0 {
		if len(errs) == 1 {
			return doc, errs[0]
		}
		return doc, errs
	}
	return doc, nil
}

// ParseStatements is a convenience wrapper around Parse.
func ParseStatements(source string) ([]Statement, error) {
	doc, err := Parse(source)
	if doc == nil {
		return nil, err
	}
	return doc.Statements, err
}

// MustParse parses source and panics if it is invalid. It is useful for fixed
// test fixtures, not for processing user input.
func MustParse(source string) *Document {
	doc, err := Parse(source)
	if err != nil {
		panic(err)
	}
	return doc
}

type syntaxParser struct {
	tokens []token
	index  int
}

func (p *syntaxParser) current() token {
	if p.index >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.index]
}

func (p *syntaxParser) previous() token {
	if p.index == 0 {
		return p.current()
	}
	return p.tokens[p.index-1]
}

func (p *syntaxParser) next() token {
	if p.index+1 >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.index+1]
}

func (p *syntaxParser) advance() token {
	t := p.current()
	if t.kind != tokenEOF {
		p.index++
	}
	return t
}

func (p *syntaxParser) atEOF() bool            { return p.current().kind == tokenEOF }
func (p *syntaxParser) atSymbol(s string) bool { return p.current().isSymbol(s) }
func (p *syntaxParser) matchSymbol(s string) bool {
	if p.atSymbol(s) {
		p.advance()
		return true
	}
	return false
}
func (p *syntaxParser) atKeyword(keyword string) bool {
	t := p.current()
	return t.kind == tokenIdentifier && strings.EqualFold(t.text, keyword)
}
func (p *syntaxParser) nextKeyword(keyword string) bool {
	t := p.next()
	return t.kind == tokenIdentifier && strings.EqualFold(t.text, keyword)
}
func (p *syntaxParser) matchKeyword(keyword string) bool {
	if p.atKeyword(keyword) {
		p.advance()
		return true
	}
	return false
}
func (p *syntaxParser) expectSymbol(symbol string) (token, error) {
	if p.atSymbol(symbol) {
		return p.advance(), nil
	}
	return token{}, p.errorAtCurrent("expected '" + symbol + "'")
}
func (p *syntaxParser) expectKeyword(keyword string) (token, error) {
	if p.atKeyword(keyword) {
		return p.advance(), nil
	}
	return token{}, p.errorAtCurrent("expected " + keyword)
}
func (p *syntaxParser) errorAtCurrent(message string) *ParseError {
	t := p.current()
	return &ParseError{Position: t.span.Start, End: t.span.End, Message: message}
}
func asParseError(err error, fallback Span) *ParseError {
	if parseError, ok := err.(*ParseError); ok {
		return parseError
	}
	return &ParseError{Position: fallback.Start, End: fallback.End, Message: err.Error()}
}

func (p *syntaxParser) recoverStatement() {
	for !p.atEOF() && !p.atSymbol(";") {
		p.advance()
	}
	p.matchSymbol(";")
}

func (p *syntaxParser) parseStatement(stopAtBrace bool) (*QueryStatement, error) {
	start := p.current().span.Start
	statement := &QueryStatement{}
	if p.matchKeyword("EXPLAIN") {
		statement.Explain = true
	}
	if p.matchKeyword("PROFILE") {
		statement.Profile = true
	}
	if !p.isClauseStart() {
		return nil, p.errorAtCurrent("expected a Cypher clause")
	}
	for p.isClauseStart() {
		clause, err := p.parseClause()
		if err != nil {
			return nil, err
		}
		statement.Clauses = append(statement.Clauses, clause)
		if stopAtBrace && p.atSymbol("}") {
			break
		}
	}
	if len(statement.Clauses) == 0 {
		return nil, p.errorAtCurrent("expected a Cypher clause")
	}
	statement.Span = Span{Start: start, End: statement.Clauses[len(statement.Clauses)-1].Location().End}
	for p.matchKeyword("UNION") {
		unionStart := p.previous().span.Start
		all := p.matchKeyword("ALL")
		branch, err := p.parseStatement(stopAtBrace)
		if err != nil {
			return nil, err
		}
		statement.UnionBranches = append(statement.UnionBranches, UnionBranch{
			Span:  Span{Start: unionStart, End: branch.Span.End},
			All:   all,
			Query: branch,
		})
		statement.Span.End = branch.Span.End
	}
	if err := validateStatement(statement); err != nil {
		return nil, err
	}
	return statement, nil
}

func (p *syntaxParser) isClauseStart() bool {
	if p.atKeyword("OPTIONAL") || p.atKeyword("MATCH") || p.atKeyword("UNWIND") ||
		p.atKeyword("WITH") || p.atKeyword("RETURN") || p.atKeyword("CREATE") ||
		p.atKeyword("MERGE") || p.atKeyword("SET") || p.atKeyword("REMOVE") ||
		p.atKeyword("DELETE") || p.atKeyword("CALL") {
		return true
	}
	return p.atKeyword("DETACH") && p.nextKeyword("DELETE")
}

func (p *syntaxParser) parseClause() (Clause, error) {
	switch {
	case p.atKeyword("OPTIONAL") || p.atKeyword("MATCH"):
		return p.parseMatch()
	case p.atKeyword("UNWIND"):
		return p.parseUnwind()
	case p.atKeyword("WITH"):
		return p.parseProjection(true)
	case p.atKeyword("RETURN"):
		return p.parseProjection(false)
	case p.atKeyword("CREATE"):
		return p.parseCreate()
	case p.atKeyword("MERGE"):
		return p.parseMerge()
	case p.atKeyword("SET"):
		return p.parseSet()
	case p.atKeyword("REMOVE"):
		return p.parseRemove()
	case p.atKeyword("DETACH") || p.atKeyword("DELETE"):
		return p.parseDelete()
	case p.atKeyword("CALL"):
		return p.parseCall()
	default:
		return nil, p.errorAtCurrent("expected Cypher clause")
	}
}

func (p *syntaxParser) parseMatch() (Clause, error) {
	start := p.current().span.Start
	optional := p.matchKeyword("OPTIONAL")
	if _, err := p.expectKeyword("MATCH"); err != nil {
		return nil, err
	}
	patterns, err := p.parsePatternList()
	if err != nil {
		return nil, err
	}
	clause := &MatchClause{Optional: optional, Patterns: patterns}
	if p.matchKeyword("WHERE") {
		clause.Where, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	end := patterns[len(patterns)-1].Span.End
	if clause.Where != nil {
		end = clause.Where.Location().End
	}
	clause.Span = Span{Start: start, End: end}
	return clause, nil
}

func (p *syntaxParser) parseUnwind() (Clause, error) {
	start := p.advance().span.Start
	expression, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if _, err = p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	alias, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	return &UnwindClause{Span: Span{Start: start, End: alias.Span.End}, Expression: expression, Alias: alias}, nil
}

func (p *syntaxParser) parseProjection(with bool) (Clause, error) {
	start := p.advance().span.Start
	clause := &ProjectionClause{With: with}
	clause.Distinct = p.matchKeyword("DISTINCT")
	items, err := p.parseProjectionItems()
	if err != nil {
		return nil, err
	}
	clause.Items = items
	if with && p.matchKeyword("WHERE") {
		clause.Where, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	if p.matchKeyword("ORDER") {
		if _, err = p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		clause.OrderBy, err = p.parseSortItems()
		if err != nil {
			return nil, err
		}
	}
	if p.matchKeyword("SKIP") {
		clause.Skip, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	if p.matchKeyword("LIMIT") {
		clause.Limit, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	end := items[len(items)-1].Span.End
	if clause.Limit != nil {
		end = clause.Limit.Location().End
	} else if clause.Skip != nil {
		end = clause.Skip.Location().End
	} else if len(clause.OrderBy) > 0 {
		end = clause.OrderBy[len(clause.OrderBy)-1].Span.End
	} else if clause.Where != nil {
		end = clause.Where.Location().End
	}
	clause.Span = Span{Start: start, End: end}
	return clause, nil
}

func (p *syntaxParser) parseProjectionItems() ([]ProjectionItem, error) {
	items := make([]ProjectionItem, 0, 1)
	for {
		start := p.current().span.Start
		item := ProjectionItem{}
		if p.matchSymbol("*") {
			item.Star = true
			item.Span = Span{Start: start, End: p.previous().span.End}
		} else {
			expression, err := p.parseExpression(0)
			if err != nil {
				return nil, err
			}
			item.Expression = expression
			if p.matchKeyword("AS") {
				alias, err := p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				item.Alias = alias
				item.Span = Span{Start: start, End: alias.Span.End}
			} else {
				item.Span = Span{Start: start, End: expression.Location().End}
			}
		}
		items = append(items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	return items, nil
}

func (p *syntaxParser) parseSortItems() ([]SortItem, error) {
	items := make([]SortItem, 0, 1)
	for {
		start := p.current().span.Start
		expression, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		item := SortItem{Expression: expression, Span: Span{Start: start, End: expression.Location().End}}
		if p.matchKeyword("ASC") {
			item.Span.End = p.previous().span.End
		} else if p.matchKeyword("DESC") {
			item.Descending = true
			item.Span.End = p.previous().span.End
		}
		items = append(items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	return items, nil
}

func (p *syntaxParser) parseCreate() (Clause, error) {
	start := p.advance().span.Start
	patterns, err := p.parsePatternList()
	if err != nil {
		return nil, err
	}
	return &CreateClause{Span: Span{Start: start, End: patterns[len(patterns)-1].Span.End}, Patterns: patterns}, nil
}

func (p *syntaxParser) parseMerge() (Clause, error) {
	start := p.advance().span.Start
	pattern, err := p.parsePatternPart()
	if err != nil {
		return nil, err
	}
	clause := &MergeClause{Pattern: pattern}
	for p.matchKeyword("ON") {
		actionStart := p.previous().span.Start
		var kind MergeActionKind
		if p.matchKeyword("CREATE") {
			kind = OnCreate
		} else if p.matchKeyword("MATCH") {
			kind = OnMatch
		} else {
			return nil, p.errorAtCurrent("expected CREATE or MATCH after ON")
		}
		if _, err = p.expectKeyword("SET"); err != nil {
			return nil, err
		}
		items, err := p.parseSetItems()
		if err != nil {
			return nil, err
		}
		clause.Actions = append(clause.Actions, MergeAction{Span: Span{Start: actionStart, End: items[len(items)-1].Span.End}, Kind: kind, Set: items})
	}
	end := pattern.Span.End
	if len(clause.Actions) > 0 {
		end = clause.Actions[len(clause.Actions)-1].Span.End
	}
	clause.Span = Span{Start: start, End: end}
	return clause, nil
}

func (p *syntaxParser) parseSet() (Clause, error) {
	start := p.advance().span.Start
	items, err := p.parseSetItems()
	if err != nil {
		return nil, err
	}
	return &SetClause{Span: Span{Start: start, End: items[len(items)-1].Span.End}, Items: items}, nil
}

func (p *syntaxParser) parseSetItems() ([]SetItem, error) {
	items := make([]SetItem, 0, 1)
	for {
		start := p.current().span.Start
		// A SET left-hand side is an lvalue. Parsing it above all infix
		// precedence is important: otherwise `n.name = 'x'` is consumed as a
		// Boolean equality expression before this routine sees its assignment.
		target, err := p.parseExpression(precedencePower + 1)
		if err != nil {
			return nil, err
		}
		item := SetItem{Target: target}
		if labels, ok := target.(*LabelExpression); ok && !p.atSymbol("=") && !p.atSymbol("+=") {
			item.Labels = labels.Labels
			item.Target = labels.Expression
			item.Span = Span{Start: start, End: labels.Location().End}
		} else {
			if p.matchSymbol("=") {
				item.Operator = "="
			} else if p.matchSymbol("+=") {
				item.Operator = "+="
			} else {
				return nil, p.errorAtCurrent("expected '=' or '+=' in SET item")
			}
			item.Value, err = p.parseExpression(0)
			if err != nil {
				return nil, err
			}
			item.Span = Span{Start: start, End: item.Value.Location().End}
		}
		items = append(items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	return items, nil
}

func (p *syntaxParser) parseRemove() (Clause, error) {
	start := p.advance().span.Start
	items := make([]RemoveItem, 0, 1)
	for {
		itemStart := p.current().span.Start
		target, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		item := RemoveItem{Target: target, Span: Span{Start: itemStart, End: target.Location().End}}
		if labels, ok := target.(*LabelExpression); ok {
			item.Target = labels.Expression
			item.Labels = labels.Labels
		}
		items = append(items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	return &RemoveClause{Span: Span{Start: start, End: items[len(items)-1].Span.End}, Items: items}, nil
}

func (p *syntaxParser) parseDelete() (Clause, error) {
	start := p.current().span.Start
	detach := p.matchKeyword("DETACH")
	if _, err := p.expectKeyword("DELETE"); err != nil {
		return nil, err
	}
	expressions := make([]Expression, 0, 1)
	for {
		expression, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		expressions = append(expressions, expression)
		if !p.matchSymbol(",") {
			break
		}
	}
	return &DeleteClause{Span: Span{Start: start, End: expressions[len(expressions)-1].Location().End}, Detach: detach, Expressions: expressions}, nil
}

func (p *syntaxParser) parseCall() (Clause, error) {
	start := p.advance().span.Start
	clause := &CallClause{}
	var err error
	if p.matchSymbol("{") {
		clause.Subquery, err = p.parseStatement(true)
		if err != nil {
			return nil, err
		}
		if _, err = p.expectSymbol("}"); err != nil {
			return nil, err
		}
	} else {
		clause.Procedure, err = p.parseQualifiedName()
		if err != nil {
			return nil, err
		}
		if _, err = p.expectSymbol("("); err != nil {
			return nil, err
		}
		if !p.atSymbol(")") {
			for {
				argument, argErr := p.parseExpression(0)
				if argErr != nil {
					return nil, argErr
				}
				clause.Arguments = append(clause.Arguments, argument)
				if !p.matchSymbol(",") {
					break
				}
			}
		}
		if _, err = p.expectSymbol(")"); err != nil {
			return nil, err
		}
	}
	if p.matchKeyword("YIELD") {
		clause.Yield, err = p.parseYieldItems()
		if err != nil {
			return nil, err
		}
		if p.matchKeyword("WHERE") {
			clause.YieldWhere, err = p.parseExpression(0)
			if err != nil {
				return nil, err
			}
		}
	}
	end := p.previous().span.End
	if clause.YieldWhere != nil {
		end = clause.YieldWhere.Location().End
	} else if len(clause.Yield) > 0 {
		end = clause.Yield[len(clause.Yield)-1].Span.End
	}
	clause.Span = Span{Start: start, End: end}
	return clause, nil
}

func (p *syntaxParser) parseYieldItems() ([]YieldItem, error) {
	items := make([]YieldItem, 0, 1)
	for {
		start := p.current().span.Start
		item := YieldItem{}
		if p.matchSymbol("*") {
			item.Star = true
			item.Span = Span{Start: start, End: p.previous().span.End}
		} else {
			name, err := p.parseIdentifier()
			if err != nil {
				return nil, err
			}
			item.Name = name
			item.Span = name.Span
			if p.matchKeyword("AS") {
				alias, err := p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				item.Alias = alias
				item.Span.End = alias.Span.End
			}
		}
		items = append(items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	return items, nil
}

func (p *syntaxParser) parsePatternList() ([]PatternPart, error) {
	patterns := make([]PatternPart, 0, 1)
	for {
		pattern, err := p.parsePatternPart()
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, pattern)
		if !p.matchSymbol(",") {
			break
		}
	}
	return patterns, nil
}

func (p *syntaxParser) parsePatternPart() (PatternPart, error) {
	start := p.current().span.Start
	part := PatternPart{}
	if isIdentifierToken(p.current()) && p.next().isSymbol("=") {
		variable, err := p.parseIdentifier()
		if err != nil {
			return part, err
		}
		part.Variable = variable
		p.advance()
	}
	element, err := p.parsePatternElement()
	if err != nil {
		return part, err
	}
	part.Element = element
	part.Span = Span{Start: start, End: element.Span.End}
	return part, nil
}

func (p *syntaxParser) parsePatternElement() (PatternElement, error) {
	start := p.current().span.Start
	first, err := p.parseNodePattern()
	if err != nil {
		return PatternElement{}, err
	}
	element := PatternElement{Nodes: []NodePattern{first}}
	for p.atSymbol("-") || p.atSymbol("<-") {
		relationship, err := p.parseRelationshipPattern()
		if err != nil {
			return PatternElement{}, err
		}
		node, err := p.parseNodePattern()
		if err != nil {
			return PatternElement{}, err
		}
		element.Relationships = append(element.Relationships, relationship)
		element.Nodes = append(element.Nodes, node)
	}
	element.Span = Span{Start: start, End: element.Nodes[len(element.Nodes)-1].Span.End}
	return element, nil
}

func (p *syntaxParser) parseNodePattern() (NodePattern, error) {
	start := p.current().span.Start
	if _, err := p.expectSymbol("("); err != nil {
		return NodePattern{}, err
	}
	node := NodePattern{}
	if isIdentifierToken(p.current()) {
		variable, err := p.parseIdentifier()
		if err != nil {
			return node, err
		}
		node.Variable = variable
	}
	for p.matchSymbol(":") {
		label, err := p.parseIdentifier()
		if err != nil {
			return node, err
		}
		node.Labels = append(node.Labels, label)
	}
	if p.atSymbol("{") || p.current().kind == tokenParameter {
		properties, err := p.parsePropertyMap()
		if err != nil {
			return node, err
		}
		node.Properties = properties
	}
	end, err := p.expectSymbol(")")
	if err != nil {
		return node, err
	}
	node.Span = Span{Start: start, End: end.span.End}
	return node, nil
}

func (p *syntaxParser) parseRelationshipPattern() (RelationshipPattern, error) {
	start := p.current().span.Start
	relationship := RelationshipPattern{}
	if p.matchSymbol("<-") {
		relationship.Direction = Incoming
	} else if p.matchSymbol("-") {
		relationship.Direction = Undirected
	} else {
		return relationship, p.errorAtCurrent("expected relationship")
	}
	if p.matchSymbol("[") {
		if isIdentifierToken(p.current()) {
			variable, err := p.parseIdentifier()
			if err != nil {
				return relationship, err
			}
			relationship.Variable = variable
		}
		for p.matchSymbol(":") {
			typeName, err := p.parseIdentifier()
			if err != nil {
				return relationship, err
			}
			relationship.Types = append(relationship.Types, typeName)
			for p.matchSymbol("|") {
				// OpenCypher accepts :TYPE|OTHER and newer implementations
				// also accept :TYPE|:OTHER. Retain both spellings as types.
				p.matchSymbol(":")
				typeName, err = p.parseIdentifier()
				if err != nil {
					return relationship, err
				}
				relationship.Types = append(relationship.Types, typeName)
			}
		}
		if p.matchSymbol("*") {
			length, err := p.parseRelationshipLength(p.previous().span.Start)
			if err != nil {
				return relationship, err
			}
			relationship.Length = length
		}
		if p.atSymbol("{") || p.current().kind == tokenParameter {
			properties, err := p.parsePropertyMap()
			if err != nil {
				return relationship, err
			}
			relationship.Properties = properties
		}
		if _, err := p.expectSymbol("]"); err != nil {
			return relationship, err
		}
	}
	if relationship.Direction == Incoming {
		if !p.matchSymbol("-") {
			return relationship, p.errorAtCurrent("expected '-' after incoming relationship")
		}
	} else if p.matchSymbol("->") {
		relationship.Direction = Outgoing
	} else if !p.matchSymbol("-") {
		return relationship, p.errorAtCurrent("expected '-' or '->' after relationship")
	}
	relationship.Span = Span{Start: start, End: p.previous().span.End}
	return relationship, nil
}

func (p *syntaxParser) parseRelationshipLength(start Position) (*RelationshipLength, error) {
	length := &RelationshipLength{Span: Span{Start: start, End: p.previous().span.End}}
	if p.matchSymbol("..") {
		if !p.atSymbol("]") && !p.atSymbol("{") {
			upper, err := p.parseExpression(0)
			if err != nil {
				return nil, err
			}
			length.Upper = upper
			length.Span.End = upper.Location().End
		}
		return length, nil
	}
	if !p.atSymbol("]") && !p.atSymbol("{") {
		lower, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		length.Lower = lower
		length.Upper = lower
		length.Exact = true
		length.Span.End = lower.Location().End
		if p.matchSymbol("..") {
			length.Exact = false
			if !p.atSymbol("]") && !p.atSymbol("{") {
				upper, err := p.parseExpression(0)
				if err != nil {
					return nil, err
				}
				length.Upper = upper
				length.Span.End = upper.Location().End
			}
		}
	}
	return length, nil
}

func (p *syntaxParser) parsePropertyMap() (Expression, error) {
	if p.current().kind == tokenParameter {
		return p.parsePrimary()
	}
	if p.atSymbol("{") {
		return p.parseMapLiteral()
	}
	return nil, p.errorAtCurrent("expected map literal or parameter")
}

func (p *syntaxParser) parseExpression(minPrecedence int) (Expression, error) {
	left, err := p.parsePrefix()
	if err != nil {
		return nil, err
	}
	for {
		left, err = p.parsePostfix(left)
		if err != nil {
			return nil, err
		}
		if p.atKeyword("IS") {
			if precedenceComparison < minPrecedence {
				break
			}
			start := left.Location().Start
			p.advance()
			not := p.matchKeyword("NOT")
			end, nullErr := p.expectKeyword("NULL")
			if nullErr != nil {
				return nil, nullErr
			}
			left = &IsNullExpression{Span: Span{Start: start, End: end.span.End}, Expression: left, Not: not}
			continue
		}
		operator, precedence, rightAssociative, ok := p.binaryOperator()
		if !ok || precedence < minPrecedence {
			break
		}
		p.consumeBinaryOperator(operator)
		nextPrecedence := precedence + 1
		if rightAssociative {
			nextPrecedence = precedence
		}
		right, rightErr := p.parseExpression(nextPrecedence)
		if rightErr != nil {
			return nil, rightErr
		}
		left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: operator, Right: right}
	}
	return left, nil
}

const (
	precedenceOr = iota + 1
	precedenceXor
	precedenceAnd
	precedenceComparison
	precedenceAdditive
	precedenceMultiplicative
	precedencePower
	precedenceUnary
)

func (p *syntaxParser) binaryOperator() (operator string, precedence int, rightAssociative, ok bool) {
	switch {
	case p.atKeyword("OR"):
		return "OR", precedenceOr, false, true
	case p.atKeyword("XOR"):
		return "XOR", precedenceXor, false, true
	case p.atKeyword("AND"):
		return "AND", precedenceAnd, false, true
	case p.atKeyword("IN"):
		return "IN", precedenceComparison, false, true
	case p.atKeyword("CONTAINS"):
		return "CONTAINS", precedenceComparison, false, true
	case p.atKeyword("STARTS") && p.nextKeyword("WITH"):
		return "STARTS WITH", precedenceComparison, false, true
	case p.atKeyword("ENDS") && p.nextKeyword("WITH"):
		return "ENDS WITH", precedenceComparison, false, true
	case p.atKeyword("NOT") && p.nextKeyword("IN"):
		return "NOT IN", precedenceComparison, false, true
	case p.atSymbol("=") || p.atSymbol("<>") || p.atSymbol("!=") || p.atSymbol("<") || p.atSymbol(">") || p.atSymbol("<=") || p.atSymbol(">=") || p.atSymbol("=~"):
		return p.current().text, precedenceComparison, false, true
	case p.atSymbol("+") || p.atSymbol("-"):
		return p.current().text, precedenceAdditive, false, true
	case p.atSymbol("*") || p.atSymbol("/") || p.atSymbol("%"):
		return p.current().text, precedenceMultiplicative, false, true
	case p.atSymbol("^"):
		return "^", precedencePower, true, true
	default:
		return "", 0, false, false
	}
}

func (p *syntaxParser) consumeBinaryOperator(operator string) {
	if operator == "STARTS WITH" || operator == "ENDS WITH" || operator == "NOT IN" {
		p.advance()
		p.advance()
		return
	}
	p.advance()
}

func (p *syntaxParser) parsePrefix() (Expression, error) {
	if p.atSymbol("+") || p.atSymbol("-") {
		operator := p.advance()
		expression, err := p.parseExpression(precedenceUnary)
		if err != nil {
			return nil, err
		}
		return &UnaryExpression{Span: Span{Start: operator.span.Start, End: expression.Location().End}, Operator: operator.text, Expression: expression}, nil
	}
	if p.matchKeyword("NOT") {
		start := p.previous().span.Start
		// Cypher's boolean NOT binds around comparison predicates, so
		// `NOT n.active = true` is represented as NOT (n.active = true),
		// while AND/OR remain outside the unary expression.
		expression, err := p.parseExpression(precedenceComparison)
		if err != nil {
			return nil, err
		}
		return &UnaryExpression{Span: Span{Start: start, End: expression.Location().End}, Operator: "NOT", Expression: expression}, nil
	}
	if p.atKeyword("CASE") {
		return p.parseCaseExpression()
	}
	if p.atKeyword("EXISTS") && p.next().isSymbol("{") {
		return p.parseExistsSubquery()
	}
	return p.parsePrimary()
}

func (p *syntaxParser) parsePostfix(expression Expression) (Expression, error) {
	for {
		switch {
		case p.matchSymbol("."):
			property, err := p.parseIdentifier()
			if err != nil {
				return nil, err
			}
			expression = &PropertyExpression{Span: Span{Start: expression.Location().Start, End: property.Span.End}, Expression: expression, Property: property}
		case p.matchSymbol("["):
			start := expression.Location().Start
			if p.matchSymbol("..") {
				var end Expression
				var err error
				if !p.atSymbol("]") {
					end, err = p.parseExpression(0)
					if err != nil {
						return nil, err
					}
				}
				closing, err := p.expectSymbol("]")
				if err != nil {
					return nil, err
				}
				expression = &SliceExpression{Span: Span{Start: start, End: closing.span.End}, Expression: expression, End: end}
				continue
			}
			index, err := p.parseExpression(0)
			if err != nil {
				return nil, err
			}
			if p.matchSymbol("..") {
				var end Expression
				if !p.atSymbol("]") {
					end, err = p.parseExpression(0)
					if err != nil {
						return nil, err
					}
				}
				closing, closeErr := p.expectSymbol("]")
				if closeErr != nil {
					return nil, closeErr
				}
				expression = &SliceExpression{Span: Span{Start: start, End: closing.span.End}, Expression: expression, Start: index, End: end}
			} else {
				closing, closeErr := p.expectSymbol("]")
				if closeErr != nil {
					return nil, closeErr
				}
				expression = &IndexExpression{Span: Span{Start: start, End: closing.span.End}, Expression: expression, Index: index}
			}
		case p.matchSymbol(":"):
			labels := make([]Identifier, 0, 1)
			label, err := p.parseIdentifier()
			if err != nil {
				return nil, err
			}
			labels = append(labels, label)
			for p.matchSymbol(":") {
				label, err = p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				labels = append(labels, label)
			}
			expression = &LabelExpression{Span: Span{Start: expression.Location().Start, End: labels[len(labels)-1].Span.End}, Expression: expression, Labels: labels}
		default:
			return expression, nil
		}
	}
}

func (p *syntaxParser) parsePrimary() (Expression, error) {
	t := p.current()
	switch t.kind {
	case tokenInteger:
		p.advance()
		value, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, &ParseError{Position: t.span.Start, End: t.span.End, Message: "invalid integer literal"}
		}
		return &Literal{Span: t.span, Kind: IntegerLiteral, Value: value}, nil
	case tokenFloat:
		p.advance()
		value, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &ParseError{Position: t.span.Start, End: t.span.End, Message: "invalid floating-point literal"}
		}
		return &Literal{Span: t.span, Kind: FloatLiteral, Value: value}, nil
	case tokenString:
		p.advance()
		return &Literal{Span: t.span, Kind: StringLiteral, Value: t.text}, nil
	case tokenParameter:
		p.advance()
		return &Parameter{Span: t.span, Name: Identifier{Span: t.span, Name: t.text}}, nil
	case tokenIdentifier, tokenQuotedIdentifier:
		if t.kind == tokenIdentifier && strings.EqualFold(t.text, "NULL") {
			p.advance()
			return &Literal{Span: t.span, Kind: NullLiteral, Value: nil}, nil
		}
		if t.kind == tokenIdentifier && (strings.EqualFold(t.text, "TRUE") || strings.EqualFold(t.text, "FALSE")) {
			p.advance()
			return &Literal{Span: t.span, Kind: BooleanLiteral, Value: strings.EqualFold(t.text, "TRUE")}, nil
		}
		return p.parseVariableOrFunction()
	case tokenSymbol:
		switch t.text {
		case "[":
			return p.parseListLiteralOrComprehension()
		case "{":
			return p.parseMapLiteral()
		case "(":
			return p.parseParenthesizedOrPattern()
		}
	}
	return nil, p.errorAtCurrent("expected expression")
}

func (p *syntaxParser) parseVariableOrFunction() (Expression, error) {
	first, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	parts := []Identifier{first}
	save := p.index
	for p.matchSymbol(".") {
		if !isIdentifierToken(p.current()) {
			p.index = save
			return &Variable{Span: first.Span, Name: first}, nil
		}
		part, partErr := p.parseIdentifier()
		if partErr != nil {
			return nil, partErr
		}
		parts = append(parts, part)
	}
	if p.atSymbol("(") {
		name := QualifiedName{Span: Span{Start: first.Span.Start, End: parts[len(parts)-1].Span.End}, Parts: parts}
		return p.parseFunctionInvocation(name)
	}
	p.index = save
	return &Variable{Span: first.Span, Name: first}, nil
}

func (p *syntaxParser) parseFunctionInvocation(name QualifiedName) (Expression, error) {
	start := name.Span.Start
	if _, err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	function := strings.ToLower(name.String())
	if function == "all" || function == "any" || function == "none" || function == "single" {
		return p.parseListPredicate(start, function)
	}
	if function == "reduce" {
		return p.parseReduceExpression(start)
	}
	invocation := &FunctionInvocation{Name: name}
	invocation.Distinct = p.matchKeyword("DISTINCT")
	if p.matchSymbol("*") {
		invocation.Star = true
	} else if !p.atSymbol(")") {
		for {
			var argument Expression
			var err error
			if (function == "shortestpath" || function == "allshortestpaths") && p.atSymbol("(") {
				pattern, patternErr := p.parsePatternElement()
				if patternErr != nil {
					return nil, patternErr
				}
				argument = &PatternExpression{Span: pattern.Span, Pattern: pattern}
			} else {
				argument, err = p.parseExpression(0)
				if err != nil {
					return nil, err
				}
			}
			invocation.Arguments = append(invocation.Arguments, argument)
			if !p.matchSymbol(",") {
				break
			}
		}
	}
	closing, err := p.expectSymbol(")")
	if err != nil {
		return nil, err
	}
	invocation.Span = Span{Start: start, End: closing.span.End}
	return invocation, nil
}

func (p *syntaxParser) parseListPredicate(start Position, operator string) (Expression, error) {
	variable, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if _, err = p.expectKeyword("IN"); err != nil {
		return nil, err
	}
	list, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if _, err = p.expectKeyword("WHERE"); err != nil {
		return nil, err
	}
	where, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	closing, err := p.expectSymbol(")")
	if err != nil {
		return nil, err
	}
	return &ListPredicate{
		Span:     Span{Start: start, End: closing.span.End},
		Operator: strings.ToUpper(operator),
		Variable: variable,
		List:     list,
		Where:    where,
	}, nil
}

func (p *syntaxParser) parseReduceExpression(start Position) (Expression, error) {
	accumulator, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if _, err = p.expectSymbol("="); err != nil {
		return nil, err
	}
	initial, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if _, err = p.expectSymbol(","); err != nil {
		return nil, err
	}
	variable, err := p.parseIdentifier()
	if err != nil {
		return nil, err
	}
	if _, err = p.expectKeyword("IN"); err != nil {
		return nil, err
	}
	list, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if _, err = p.expectSymbol("|"); err != nil {
		return nil, err
	}
	expression, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	closing, err := p.expectSymbol(")")
	if err != nil {
		return nil, err
	}
	return &ReduceExpression{
		Span:        Span{Start: start, End: closing.span.End},
		Accumulator: accumulator,
		Initial:     initial,
		Variable:    variable,
		List:        list,
		Expression:  expression,
	}, nil
}

func (p *syntaxParser) parseListLiteralOrComprehension() (Expression, error) {
	start := p.current().span.Start
	p.advance() // [
	if p.matchSymbol("]") {
		return &ListLiteral{Span: Span{Start: start, End: p.previous().span.End}}, nil
	}
	if isIdentifierToken(p.current()) && p.nextKeyword("IN") {
		variable, err := p.parseIdentifier()
		if err != nil {
			return nil, err
		}
		p.advance() // IN
		list, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		comprehension := &ListComprehension{Variable: variable, List: list}
		if p.matchKeyword("WHERE") {
			comprehension.Where, err = p.parseExpression(0)
			if err != nil {
				return nil, err
			}
		}
		if p.matchSymbol("|") {
			comprehension.Projection, err = p.parseExpression(0)
			if err != nil {
				return nil, err
			}
		}
		closing, err := p.expectSymbol("]")
		if err != nil {
			return nil, err
		}
		comprehension.Span = Span{Start: start, End: closing.span.End}
		return comprehension, nil
	}
	literal := &ListLiteral{}
	for {
		element, err := p.parseExpression(0)
		if err != nil {
			return nil, err
		}
		literal.Elements = append(literal.Elements, element)
		if !p.matchSymbol(",") {
			break
		}
	}
	closing, err := p.expectSymbol("]")
	if err != nil {
		return nil, err
	}
	literal.Span = Span{Start: start, End: closing.span.End}
	return literal, nil
}

func (p *syntaxParser) parseMapLiteral() (Expression, error) {
	start := p.current().span.Start
	if _, err := p.expectSymbol("{"); err != nil {
		return nil, err
	}
	mapLiteral := &MapLiteral{}
	if !p.atSymbol("}") {
		for {
			entryStart := p.current().span.Start
			var key Identifier
			if p.current().kind == tokenString {
				t := p.advance()
				key = Identifier{Span: t.span, Name: t.text}
			} else {
				parsed, err := p.parseIdentifier()
				if err != nil {
					return nil, err
				}
				key = parsed
			}
			if _, err := p.expectSymbol(":"); err != nil {
				return nil, err
			}
			value, err := p.parseExpression(0)
			if err != nil {
				return nil, err
			}
			mapLiteral.Entries = append(mapLiteral.Entries, MapEntry{Span: Span{Start: entryStart, End: value.Location().End}, Key: key, Value: value})
			if !p.matchSymbol(",") {
				break
			}
		}
	}
	closing, err := p.expectSymbol("}")
	if err != nil {
		return nil, err
	}
	mapLiteral.Span = Span{Start: start, End: closing.span.End}
	return mapLiteral, nil
}

func (p *syntaxParser) parseParenthesizedOrPattern() (Expression, error) {
	save := p.index
	pattern, err := p.parsePatternElement()
	if err == nil && len(pattern.Relationships) > 0 {
		return &PatternExpression{Span: pattern.Span, Pattern: pattern}, nil
	}
	p.index = save
	if _, err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	expression, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if _, err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return expression, nil
}

func (p *syntaxParser) parseCaseExpression() (Expression, error) {
	start := p.advance().span.Start
	caseExpression := &CaseExpression{}
	var err error
	if !p.atKeyword("WHEN") {
		caseExpression.Operand, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	for p.matchKeyword("WHEN") {
		whenStart := p.previous().span.Start
		when, whenErr := p.parseExpression(0)
		if whenErr != nil {
			return nil, whenErr
		}
		if _, thenErr := p.expectKeyword("THEN"); thenErr != nil {
			return nil, thenErr
		}
		then, thenErr := p.parseExpression(0)
		if thenErr != nil {
			return nil, thenErr
		}
		caseExpression.Alternatives = append(caseExpression.Alternatives, CaseAlternative{Span: Span{Start: whenStart, End: then.Location().End}, When: when, Then: then})
	}
	if len(caseExpression.Alternatives) == 0 {
		return nil, p.errorAtCurrent("CASE requires at least one WHEN branch")
	}
	if p.matchKeyword("ELSE") {
		caseExpression.Else, err = p.parseExpression(0)
		if err != nil {
			return nil, err
		}
	}
	end, err := p.expectKeyword("END")
	if err != nil {
		return nil, err
	}
	caseExpression.Span = Span{Start: start, End: end.span.End}
	return caseExpression, nil
}

func (p *syntaxParser) parseExistsSubquery() (Expression, error) {
	start := p.advance().span.Start
	if _, err := p.expectSymbol("{"); err != nil {
		return nil, err
	}
	subquery, err := p.parseStatement(true)
	if err != nil {
		return nil, err
	}
	end, err := p.expectSymbol("}")
	if err != nil {
		return nil, err
	}
	return &ExistsSubquery{Span: Span{Start: start, End: end.span.End}, Subquery: subquery}, nil
}

func (p *syntaxParser) parseIdentifier() (Identifier, error) {
	t := p.current()
	if !isIdentifierToken(t) {
		return Identifier{}, p.errorAtCurrent("expected identifier")
	}
	p.advance()
	return Identifier{Span: t.span, Name: t.text, BacktickQuoted: t.kind == tokenQuotedIdentifier}, nil
}

func isIdentifierToken(t token) bool {
	return t.kind == tokenIdentifier || t.kind == tokenQuotedIdentifier
}

func (p *syntaxParser) parseQualifiedName() (QualifiedName, error) {
	first, err := p.parseIdentifier()
	if err != nil {
		return QualifiedName{}, err
	}
	name := QualifiedName{Span: first.Span, Parts: []Identifier{first}}
	for p.matchSymbol(".") {
		part, partErr := p.parseIdentifier()
		if partErr != nil {
			return QualifiedName{}, partErr
		}
		name.Parts = append(name.Parts, part)
		name.Span.End = part.Span.End
	}
	return name, nil
}

func validateStatement(statement *QueryStatement) error {
	for i, clause := range statement.Clauses {
		projection, ok := clause.(*ProjectionClause)
		if ok && !projection.With && i != len(statement.Clauses)-1 {
			return &ParseError{Position: projection.Span.Start, End: projection.Span.End, Message: "RETURN must be the final clause"}
		}
	}
	return nil
}

// String gives a compact debugging representation of a position.
func (p Position) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Column) }
