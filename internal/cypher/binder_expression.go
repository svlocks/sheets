package cypher

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/antlr4-go/antlr/v4"
	"github.com/svlocks/sheets/internal/cypher/parsergen"
)

func (b *cstBinder) bindExpression(ctx parsergen.IOC_ExpressionContext) (Expression, error) {
	if ctx == nil || ctx.OC_OrExpression() == nil {
		return nil, b.unsupported(ctx, "expression", "the generated CST contained no expression")
	}
	expression, err := b.bindOrExpression(ctx.OC_OrExpression())
	if err != nil {
		return nil, err
	}
	// Lower-precedence rules frequently collapse to their only child. Retain
	// the complete oC_Expression range nevertheless: implicit projection names
	// and diagnostics must not lose parenthesized source text.
	if !setExpressionSpan(expression, b.span(ctx)) {
		return nil, binderInvariant(ctx, "unhandled expression type %T", expression)
	}
	return expression, nil
}

func setExpressionSpan(expression Expression, span Span) bool {
	switch expression := expression.(type) {
	case *Literal:
		expression.Span = span
	case *Variable:
		expression.Span = span
	case *Parameter:
		expression.Span = span
	case *UnaryExpression:
		expression.Span = span
	case *BinaryExpression:
		expression.Span = span
	case *IsNullExpression:
		expression.Span = span
	case *PropertyExpression:
		expression.Span = span
	case *LabelExpression:
		expression.Span = span
	case *IndexExpression:
		expression.Span = span
	case *SliceExpression:
		expression.Span = span
	case *FunctionInvocation:
		expression.Span = span
	case *ListLiteral:
		expression.Span = span
	case *MapLiteral:
		expression.Span = span
	case *CaseExpression:
		expression.Span = span
	case *ListComprehension:
		expression.Span = span
	case *PatternComprehension:
		expression.Span = span
	case *ListPredicate:
		expression.Span = span
	case *ReduceExpression:
		expression.Span = span
	case *PatternExpression:
		expression.Span = span
	case *ExistsSubquery:
		expression.Span = span
	default:
		return false
	}
	return true
}

func (b *cstBinder) bindOrExpression(ctx parsergen.IOC_OrExpressionContext) (Expression, error) {
	operands := ctx.AllOC_XorExpression()
	return bindBinarySequence(operands, "OR", b.bindXorExpression)
}

func (b *cstBinder) bindXorExpression(ctx parsergen.IOC_XorExpressionContext) (Expression, error) {
	operands := ctx.AllOC_AndExpression()
	return bindBinarySequence(operands, "XOR", b.bindAndExpression)
}

func (b *cstBinder) bindAndExpression(ctx parsergen.IOC_AndExpressionContext) (Expression, error) {
	operands := ctx.AllOC_NotExpression()
	return bindBinarySequence(operands, "AND", b.bindNotExpression)
}

func bindBinarySequence[T antlr.ParserRuleContext](operands []T, operator string, bind func(T) (Expression, error)) (Expression, error) {
	if len(operands) == 0 {
		return nil, binderInvariant(nilRule{}, "empty %s expression", operator)
	}
	left, err := bind(operands[0])
	if err != nil {
		return nil, err
	}
	for _, operand := range operands[1:] {
		right, rightErr := bind(operand)
		if rightErr != nil {
			return nil, rightErr
		}
		left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: operator, Right: right}
	}
	return left, nil
}

// nilRule exists only to make an impossible generated-CST invariant printable.
// Successful generated parses never take this path.
type nilRule struct{ antlr.ParserRuleContext }

func (n nilRule) GetText() string { return "<missing>" }

func (b *cstBinder) bindNotExpression(ctx parsergen.IOC_NotExpressionContext) (Expression, error) {
	expression, err := b.bindComparisonExpression(ctx.OC_ComparisonExpression())
	if err != nil {
		return nil, err
	}
	nots := ctx.AllNOT()
	for index := len(nots) - 1; index >= 0; index-- {
		start := b.mapper.tokenSpan(nots[index].GetSymbol()).Start
		expression = &UnaryExpression{Span: Span{Start: start, End: expression.Location().End}, Operator: "NOT", Expression: expression}
	}
	return expression, nil
}

func (b *cstBinder) bindComparisonExpression(ctx parsergen.IOC_ComparisonExpressionContext) (Expression, error) {
	left, err := b.bindPredicateExpression(ctx.OC_StringListNullPredicateExpression())
	if err != nil {
		return nil, err
	}
	var comparisonTail Expression
	for _, partial := range ctx.AllOC_PartialComparisonExpression() {
		right, rightErr := b.bindPredicateExpression(partial.OC_StringListNullPredicateExpression())
		if rightErr != nil {
			return nil, rightErr
		}
		operator := comparisonOperator(partial)
		if comparisonTail == nil {
			left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: operator, Right: right}
		} else {
			comparison := &BinaryExpression{Span: Span{Start: comparisonTail.Location().Start, End: right.Location().End}, Left: comparisonTail, Operator: operator, Right: right}
			left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: "AND", Right: comparison}
		}
		comparisonTail = right
	}
	return left, nil
}

func comparisonOperator(ctx parsergen.IOC_PartialComparisonExpressionContext) string {
	for _, operator := range []string{"=", "<>", "!=", "=~", "<=", ">=", "<", ">"} {
		if directToken(ctx, operator) != nil {
			return operator
		}
	}
	return ""
}

func (b *cstBinder) bindPredicateExpression(ctx parsergen.IOC_StringListNullPredicateExpressionContext) (Expression, error) {
	expression, err := b.bindAddExpression(ctx.OC_AddOrSubtractExpression())
	if err != nil {
		return nil, err
	}
	for _, child := range ctx.GetChildren() {
		switch predicate := child.(type) {
		case parsergen.IOC_AddOrSubtractExpressionContext:
			// The leading value was bound above.
		case parsergen.IOC_StringPredicateExpressionContext:
			right, rightErr := b.bindAddExpression(predicate.OC_AddOrSubtractExpression())
			if rightErr != nil {
				return nil, rightErr
			}
			operator := "CONTAINS"
			if predicate.STARTS() != nil {
				operator = "STARTS WITH"
			} else if predicate.ENDS() != nil {
				operator = "ENDS WITH"
			}
			expression = &BinaryExpression{Span: Span{Start: expression.Location().Start, End: right.Location().End}, Left: expression, Operator: operator, Right: right}
		case parsergen.IOC_ListPredicateExpressionContext:
			right, rightErr := b.bindAddExpression(predicate.OC_AddOrSubtractExpression())
			if rightErr != nil {
				return nil, rightErr
			}
			operator := "IN"
			if predicate.NOT() != nil {
				operator = "NOT IN"
			}
			expression = &BinaryExpression{Span: Span{Start: expression.Location().Start, End: right.Location().End}, Left: expression, Operator: operator, Right: right}
		case parsergen.IOC_NullPredicateExpressionContext:
			expression = &IsNullExpression{Span: Span{Start: expression.Location().Start, End: b.span(predicate).End}, Expression: expression, Not: predicate.NOT() != nil}
		default:
			if unhandled, ok := child.(antlr.ParserRuleContext); ok {
				return nil, b.unsupported(unhandled, "predicate operator", "the generated CST contains an unhandled predicate child")
			}
		}
	}
	return expression, nil
}

func (b *cstBinder) bindAddExpression(ctx parsergen.IOC_AddOrSubtractExpressionContext) (Expression, error) {
	operands := ctx.AllOC_MultiplyDivideModuloExpression()
	if len(operands) == 0 {
		return nil, binderInvariant(ctx, "empty additive expression")
	}
	left, err := b.bindMultiplyExpression(operands[0])
	if err != nil {
		return nil, err
	}
	for index, operand := range operands[1:] {
		right, rightErr := b.bindMultiplyExpression(operand)
		if rightErr != nil {
			return nil, rightErr
		}
		operator := b.operatorBetween(operands[index], operand)
		left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: operator, Right: right}
	}
	return left, nil
}

func (b *cstBinder) bindMultiplyExpression(ctx parsergen.IOC_MultiplyDivideModuloExpressionContext) (Expression, error) {
	operands := ctx.AllOC_PowerOfExpression()
	if len(operands) == 0 {
		return nil, binderInvariant(ctx, "empty multiplicative expression")
	}
	left, err := b.bindPowerExpression(operands[0])
	if err != nil {
		return nil, err
	}
	for index, operand := range operands[1:] {
		right, rightErr := b.bindPowerExpression(operand)
		if rightErr != nil {
			return nil, rightErr
		}
		operator := b.operatorBetween(operands[index], operand)
		left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: operator, Right: right}
	}
	return left, nil
}

func (b *cstBinder) bindPowerExpression(ctx parsergen.IOC_PowerOfExpressionContext) (Expression, error) {
	operands := ctx.AllOC_UnaryAddOrSubtractExpression()
	if len(operands) == 0 {
		return nil, binderInvariant(ctx, "empty power expression")
	}
	left, err := b.bindUnaryExpression(operands[0])
	if err != nil {
		return nil, err
	}
	for _, operand := range operands[1:] {
		right, rightErr := b.bindUnaryExpression(operand)
		if rightErr != nil {
			return nil, rightErr
		}
		left = &BinaryExpression{Span: Span{Start: left.Location().Start, End: right.Location().End}, Left: left, Operator: "^", Right: right}
	}
	return left, nil
}

func (b *cstBinder) bindUnaryExpression(ctx parsergen.IOC_UnaryAddOrSubtractExpressionContext) (Expression, error) {
	operator := ""
	if directToken(ctx, "+") != nil {
		operator = "+"
	} else if directToken(ctx, "-") != nil {
		operator = "-"
	}
	nonArithmetic := ctx.OC_NonArithmeticOperatorExpression()
	if operator != "" {
		if integer := bareIntegerLiteral(nonArithmetic); integer != nil {
			return b.bindIntegerLiteral(integer, operator == "-", b.span(ctx))
		}
	}
	expression, err := b.bindNonArithmeticExpression(nonArithmetic)
	if err != nil || operator == "" {
		return expression, err
	}
	return &UnaryExpression{Span: b.span(ctx), Operator: operator, Expression: expression}, nil
}

func bareIntegerLiteral(ctx parsergen.IOC_NonArithmeticOperatorExpressionContext) parsergen.IOC_IntegerLiteralContext {
	if ctx == nil || len(ctx.AllOC_ListOperatorExpression()) != 0 || len(ctx.AllOC_PropertyLookup()) != 0 || ctx.OC_NodeLabels() != nil {
		return nil
	}
	atom := ctx.OC_Atom()
	if atom == nil || atom.OC_Literal() == nil || atom.OC_Literal().OC_NumberLiteral() == nil {
		return nil
	}
	return atom.OC_Literal().OC_NumberLiteral().OC_IntegerLiteral()
}

func (b *cstBinder) bindNonArithmeticExpression(ctx parsergen.IOC_NonArithmeticOperatorExpressionContext) (Expression, error) {
	expression, err := b.bindAtom(ctx.OC_Atom())
	if err != nil {
		return nil, err
	}
	for _, child := range ctx.GetChildren() {
		switch operator := child.(type) {
		case parsergen.IOC_AtomContext, parsergen.IOC_NodeLabelsContext:
			// The atom and trailing labels are bound outside this loop.
		case parsergen.IOC_PropertyLookupContext:
			property := b.bindIdentifier(operator.OC_PropertyKeyName().OC_SchemaName())
			expression = &PropertyExpression{Span: Span{Start: expression.Location().Start, End: b.span(operator).End}, Expression: expression, Property: property}
		case parsergen.IOC_ListOperatorExpressionContext:
			expression, err = b.bindListOperator(expression, operator)
			if err != nil {
				return nil, err
			}
		default:
			if unhandled, ok := child.(antlr.ParserRuleContext); ok {
				return nil, b.unsupported(unhandled, "non-arithmetic operator", "the generated CST contains an unhandled expression child")
			}
		}
	}
	if labels := ctx.OC_NodeLabels(); labels != nil {
		expression = &LabelExpression{Span: Span{Start: expression.Location().Start, End: b.span(labels).End}, Expression: expression, Labels: b.bindNodeLabels(labels)}
	}
	return expression, nil
}

func (b *cstBinder) bindListOperator(base Expression, ctx parsergen.IOC_ListOperatorExpressionContext) (Expression, error) {
	expressions := ctx.AllOC_Expression()
	if !strings.Contains(ctx.GetText(), "..") {
		index, err := b.bindExpression(expressions[0])
		if err != nil {
			return nil, err
		}
		return &IndexExpression{Span: Span{Start: base.Location().Start, End: b.span(ctx).End}, Expression: base, Index: index}, nil
	}
	slice := &SliceExpression{Span: Span{Start: base.Location().Start, End: b.span(ctx).End}, Expression: base}
	rangeToken := directToken(ctx, "..")
	rangeStart := rangeToken.GetSymbol().GetStart()
	for _, expressionCtx := range expressions {
		expression, err := b.bindExpression(expressionCtx)
		if err != nil {
			return nil, err
		}
		if expressionCtx.GetStart().GetStart() < rangeStart {
			slice.Start = expression
		} else {
			slice.End = expression
		}
	}
	return slice, nil
}

func (b *cstBinder) bindAtom(ctx parsergen.IOC_AtomContext) (Expression, error) {
	switch {
	case ctx.OC_Literal() != nil:
		return b.bindLiteral(ctx.OC_Literal())
	case ctx.OC_Parameter() != nil:
		return b.bindParameter(ctx.OC_Parameter()), nil
	case ctx.OC_CaseExpression() != nil:
		return b.bindCaseExpression(ctx.OC_CaseExpression())
	case ctx.COUNT() != nil:
		span := b.span(ctx)
		identifier := Identifier{Span: b.mapper.tokenSpan(ctx.COUNT().GetSymbol()), Name: ctx.COUNT().GetText()}
		return &FunctionInvocation{Span: span, Name: QualifiedName{Span: identifier.Span, Parts: []Identifier{identifier}}, Star: true}, nil
	case ctx.OC_ListComprehension() != nil:
		return b.bindListComprehension(ctx.OC_ListComprehension())
	case ctx.OC_PatternComprehension() != nil:
		return b.bindPatternComprehension(ctx.OC_PatternComprehension())
	case ctx.OC_Quantifier() != nil:
		return b.bindQuantifier(ctx.OC_Quantifier())
	case ctx.OC_Reduce() != nil:
		return b.bindReduce(ctx.OC_Reduce())
	case ctx.OC_PatternPredicate() != nil:
		element, err := b.bindRelationshipsPattern(ctx.OC_PatternPredicate().OC_RelationshipsPattern())
		if err != nil {
			return nil, err
		}
		return &PatternExpression{Span: b.span(ctx.OC_PatternPredicate()), Pattern: element}, nil
	case ctx.OC_ParenthesizedExpression() != nil:
		return b.bindExpression(ctx.OC_ParenthesizedExpression().OC_Expression())
	case ctx.OC_FunctionInvocation() != nil:
		return b.bindFunctionInvocation(ctx.OC_FunctionInvocation())
	case ctx.OC_ExistentialSubquery() != nil:
		return b.bindExistentialSubquery(ctx.OC_ExistentialSubquery())
	case ctx.OC_Variable() != nil:
		return b.bindVariableExpression(ctx.OC_Variable()), nil
	default:
		return nil, b.unsupported(ctx, "expression atom", "the recognized atom has no AST binding")
	}
}

func (b *cstBinder) bindVariableExpression(ctx parsergen.IOC_VariableContext) Expression {
	identifier := b.bindVariableIdentifier(ctx)
	return &Variable{Span: b.span(ctx), Name: identifier}
}

func (b *cstBinder) bindPropertyExpression(ctx parsergen.IOC_PropertyExpressionContext) (Expression, error) {
	expression, err := b.bindAtom(ctx.OC_Atom())
	if err != nil {
		return nil, err
	}
	for _, lookup := range ctx.AllOC_PropertyLookup() {
		property := b.bindIdentifier(lookup.OC_PropertyKeyName().OC_SchemaName())
		expression = &PropertyExpression{Span: Span{Start: expression.Location().Start, End: b.span(lookup).End}, Expression: expression, Property: property}
	}
	return expression, nil
}

func (b *cstBinder) bindLiteral(ctx parsergen.IOC_LiteralContext) (Expression, error) {
	span := b.span(ctx)
	switch {
	case ctx.NULL() != nil:
		return &Literal{Span: span, Kind: NullLiteral, Value: nil}, nil
	case ctx.OC_BooleanLiteral() != nil:
		return &Literal{Span: span, Kind: BooleanLiteral, Value: ctx.OC_BooleanLiteral().TRUE() != nil}, nil
	case ctx.OC_NumberLiteral() != nil:
		number := ctx.OC_NumberLiteral()
		if integer := number.OC_IntegerLiteral(); integer != nil {
			return b.bindIntegerLiteral(integer, false, span)
		}
		value, err := strconv.ParseFloat(number.OC_DoubleLiteral().GetText(), 64)
		if err != nil {
			return nil, &ParseError{Position: span.Start, End: span.End, Message: "invalid floating-point literal"}
		}
		return &Literal{Span: span, Kind: FloatLiteral, Value: value}, nil
	case ctx.StringLiteral() != nil:
		value, err := decodeStringLiteral(ctx.StringLiteral().GetText())
		if err != nil {
			return nil, &ParseError{Position: span.Start, End: span.End, Message: err.Error()}
		}
		return &Literal{Span: span, Kind: StringLiteral, Value: value}, nil
	case ctx.OC_ListLiteral() != nil:
		return b.bindListLiteral(ctx.OC_ListLiteral())
	case ctx.OC_MapLiteral() != nil:
		return b.bindMapLiteral(ctx.OC_MapLiteral())
	default:
		return nil, b.unsupported(ctx, "literal", "the recognized literal has no AST binding")
	}
}

func (b *cstBinder) bindIntegerLiteral(ctx parsergen.IOC_IntegerLiteralContext, negative bool, span Span) (Expression, error) {
	value, err := parseIntegerLiteral(ctx.GetText(), negative)
	if err != nil {
		return nil, &ParseError{Position: span.Start, End: span.End, Message: "invalid integer literal"}
	}
	return &Literal{Span: span, Kind: IntegerLiteral, Value: value}, nil
}

func parseIntegerLiteral(raw string, negative bool) (int64, error) {
	base := 10
	digits := raw
	if len(raw) >= 2 && raw[0] == '0' {
		switch raw[1] {
		case 'x':
			base, digits = 16, raw[2:]
		case 'o':
			base, digits = 8, raw[2:]
		}
	}
	magnitude, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, err
	}
	if negative {
		if magnitude > uint64(math.MaxInt64)+1 {
			return 0, strconv.ErrRange
		}
		if magnitude == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -int64(magnitude), nil
	}
	if magnitude > math.MaxInt64 {
		return 0, strconv.ErrRange
	}
	return int64(magnitude), nil
}

func (b *cstBinder) bindParameter(ctx parsergen.IOC_ParameterContext) Expression {
	span := b.span(ctx)
	name := ""
	quoted := false
	if symbolic := ctx.OC_SymbolicName(); symbolic != nil {
		identifier := b.bindIdentifier(symbolic)
		name, quoted = identifier.Name, identifier.BacktickQuoted
	} else if reserved := ctx.OC_ReservedWord(); reserved != nil {
		name = reserved.GetText()
	} else if decimal := ctx.DecimalInteger(); decimal != nil {
		name = decimal.GetText()
	}
	return &Parameter{Span: span, Name: Identifier{Span: span, Name: name, BacktickQuoted: quoted}}
}

func (b *cstBinder) bindListLiteral(ctx parsergen.IOC_ListLiteralContext) (Expression, error) {
	literal := &ListLiteral{Span: b.span(ctx)}
	for _, expressionCtx := range ctx.AllOC_Expression() {
		expression, err := b.bindExpression(expressionCtx)
		if err != nil {
			return nil, err
		}
		literal.Elements = append(literal.Elements, expression)
	}
	return literal, nil
}

func (b *cstBinder) bindMapLiteral(ctx parsergen.IOC_MapLiteralContext) (Expression, error) {
	literal := &MapLiteral{Span: b.span(ctx)}
	keys := ctx.AllOC_PropertyKeyName()
	values := ctx.AllOC_Expression()
	if len(keys) != len(values) {
		return nil, binderInvariant(ctx, "map key/value count differs: %d/%d", len(keys), len(values))
	}
	for index, keyCtx := range keys {
		value, err := b.bindExpression(values[index])
		if err != nil {
			return nil, err
		}
		key := b.bindIdentifier(keyCtx.OC_SchemaName())
		literal.Entries = append(literal.Entries, MapEntry{Span: Span{Start: key.Span.Start, End: value.Location().End}, Key: key, Value: value})
	}
	return literal, nil
}

func (b *cstBinder) bindFunctionInvocation(ctx parsergen.IOC_FunctionInvocationContext) (Expression, error) {
	name := b.bindFunctionName(ctx.OC_FunctionName())
	normalized := strings.ToLower(name.String())
	if !catalogFunctionName(name) {
		return nil, b.unsupported(ctx, "function invocation", "function "+name.String()+" is not in sheets's supported function catalog")
	}
	if ctx.DISTINCT() != nil && IsSupportedFunction(normalized) && !aggregateFunction(normalized) {
		return nil, b.unsupported(ctx, "DISTINCT scalar function", "DISTINCT is implemented only for aggregate functions")
	}
	invocation := &FunctionInvocation{Span: b.span(ctx), Name: name, Distinct: ctx.DISTINCT() != nil}
	for _, argumentCtx := range ctx.AllOC_Expression() {
		argument, err := b.bindExpression(argumentCtx)
		if err != nil {
			return nil, err
		}
		invocation.Arguments = append(invocation.Arguments, argument)
	}
	return invocation, nil
}

// catalogFunctionName rejects escaped single-part identifiers that merely
// render like a qualified built-in (for example `date.truncate`). Qualified
// names retain their parts in the AST, so collapsing such a name with
// QualifiedName.String would otherwise alias a distinct function to a
// namespaced built-in.
func catalogFunctionName(name QualifiedName) bool {
	if len(name.Parts) == 0 {
		return false
	}
	for _, part := range name.Parts {
		if strings.ContainsRune(part.Name, '.') {
			return false
		}
	}
	return true
}

// IsSupportedFunction reports whether name belongs to the runtime built-in
// catalog. Unknown syntactically valid names still bind so semantic validation
// can report UnknownFunction before query execution begins.
func IsSupportedFunction(name string) bool {
	switch name {
	case "count", "collect", "sum", "avg", "min", "max", "stdev", "stdevp", "percentilecont", "percentiledisc",
		"coalesce", "id", "elementid", "labels", "type", "properties", "keys", "body", "nodes", "relationships",
		"startnode", "endnode", "exists", "shortestpath", "allshortestpaths", "size", "length", "head", "last", "tail",
		"tostring", "tointeger", "tofloat", "toboolean", "tolower", "lower", "toupper", "upper", "trim", "ltrim", "rtrim",
		"reverse", "replace", "substring", "range", "timestamp", "datetime", "localdatetime", "date", "time", "localtime",
		"duration", "datetime.fromepoch", "datetime.fromepochmillis", "date.truncate", "datetime.truncate",
		"localdatetime.truncate", "time.truncate", "localtime.truncate", "duration.between", "duration.inmonths",
		"duration.indays", "duration.inseconds", "randomuuid", "abs", "ceil", "sqrt", "sign", "split", "rand",
		"date.transaction", "date.statement", "date.realtime",
		"localtime.transaction", "localtime.statement", "localtime.realtime",
		"time.transaction", "time.statement", "time.realtime",
		"localdatetime.transaction", "localdatetime.statement", "localdatetime.realtime",
		"datetime.transaction", "datetime.statement", "datetime.realtime":
		return true
	default:
		return false
	}
}

func aggregateFunction(name string) bool {
	switch name {
	case "count", "collect", "sum", "avg", "min", "max", "stdev", "stdevp", "percentilecont", "percentiledisc":
		return true
	default:
		return false
	}
}

func (b *cstBinder) bindCaseExpression(ctx parsergen.IOC_CaseExpressionContext) (Expression, error) {
	result := &CaseExpression{Span: b.span(ctx)}
	directExpressions := ctx.AllOC_Expression()
	alternatives := ctx.AllOC_CaseAlternative()
	if len(alternatives) == 0 {
		return nil, binderInvariant(ctx, "CASE has no alternatives")
	}
	if len(directExpressions) != 0 && directExpressions[0].GetStart().GetTokenIndex() < alternatives[0].GetStart().GetTokenIndex() {
		var err error
		result.Operand, err = b.bindExpression(directExpressions[0])
		if err != nil {
			return nil, err
		}
		directExpressions = directExpressions[1:]
	}
	for _, alternativeCtx := range alternatives {
		expressions := alternativeCtx.AllOC_Expression()
		when, err := b.bindExpression(expressions[0])
		if err != nil {
			return nil, err
		}
		then, err := b.bindExpression(expressions[1])
		if err != nil {
			return nil, err
		}
		result.Alternatives = append(result.Alternatives, CaseAlternative{Span: b.span(alternativeCtx), When: when, Then: then})
	}
	if ctx.ELSE() != nil && len(directExpressions) != 0 {
		var err error
		result.Else, err = b.bindExpression(directExpressions[len(directExpressions)-1])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (b *cstBinder) bindListComprehension(ctx parsergen.IOC_ListComprehensionContext) (Expression, error) {
	filter := ctx.OC_FilterExpression()
	id := filter.OC_IdInColl()
	list, err := b.bindExpression(id.OC_Expression())
	if err != nil {
		return nil, err
	}
	result := &ListComprehension{Span: b.span(ctx), Variable: b.bindVariableIdentifier(id.OC_Variable()), List: list}
	if where := filter.OC_Where(); where != nil {
		result.Where, err = b.bindExpression(where.OC_Expression())
		if err != nil {
			return nil, err
		}
	}
	if projection := ctx.OC_Expression(); projection != nil {
		result.Projection, err = b.bindExpression(projection)
	}
	return result, err
}

func (b *cstBinder) bindPatternComprehension(ctx parsergen.IOC_PatternComprehensionContext) (Expression, error) {
	pattern, err := b.bindRelationshipsPattern(ctx.OC_RelationshipsPattern())
	if err != nil {
		return nil, err
	}
	result := &PatternComprehension{Span: b.span(ctx), Pattern: pattern}
	if variable := ctx.OC_Variable(); variable != nil {
		result.Variable = b.bindVariableIdentifier(variable)
	}
	if where := ctx.OC_Where(); where != nil {
		result.Where, err = b.bindExpression(where.OC_Expression())
		if err != nil {
			return nil, err
		}
	}
	if projection := ctx.OC_Expression(); projection != nil {
		result.Projection, err = b.bindExpression(projection)
	} else {
		return nil, binderInvariant(ctx, "pattern comprehension has no projection")
	}
	return result, err
}

func (b *cstBinder) bindQuantifier(ctx parsergen.IOC_QuantifierContext) (Expression, error) {
	filter := ctx.OC_FilterExpression()
	id := filter.OC_IdInColl()
	list, err := b.bindExpression(id.OC_Expression())
	if err != nil {
		return nil, err
	}
	operator := "ALL"
	if ctx.ANY() != nil {
		operator = "ANY"
	} else if ctx.NONE() != nil {
		operator = "NONE"
	} else if ctx.SINGLE() != nil {
		operator = "SINGLE"
	}
	variable := b.bindVariableIdentifier(id.OC_Variable())
	result := &ListPredicate{Span: b.span(ctx), Operator: operator, Variable: variable, List: list}
	if where := filter.OC_Where(); where != nil {
		result.Where, err = b.bindExpression(where.OC_Expression())
	} else {
		result.Where = &Variable{Span: variable.Span, Name: variable}
	}
	return result, err
}

func (b *cstBinder) bindReduce(ctx parsergen.IOC_ReduceContext) (Expression, error) {
	expressions := ctx.AllOC_Expression()
	if len(expressions) != 2 {
		return nil, binderInvariant(ctx, "reduce direct expression count = %d", len(expressions))
	}
	initial, err := b.bindExpression(expressions[0])
	if err != nil {
		return nil, err
	}
	id := ctx.OC_IdInColl()
	list, err := b.bindExpression(id.OC_Expression())
	if err != nil {
		return nil, err
	}
	resultExpression, err := b.bindExpression(expressions[1])
	if err != nil {
		return nil, err
	}
	return &ReduceExpression{
		Span:        b.span(ctx),
		Accumulator: b.bindVariableIdentifier(ctx.OC_Variable()),
		Initial:     initial,
		Variable:    b.bindVariableIdentifier(id.OC_Variable()),
		List:        list,
		Expression:  resultExpression,
	}, nil
}

func (b *cstBinder) bindExistentialSubquery(ctx parsergen.IOC_ExistentialSubqueryContext) (Expression, error) {
	var (
		query *QueryStatement
		err   error
	)
	switch {
	case ctx.OC_RegularQuery() != nil:
		query, err = b.bindRegularQuery(ctx.OC_RegularQuery())
	case ctx.OC_ExistentialMatchQuery() != nil:
		query = &QueryStatement{Span: b.span(ctx.OC_ExistentialMatchQuery())}
		for _, matchCtx := range ctx.OC_ExistentialMatchQuery().AllOC_Match() {
			clause, matchErr := b.bindMatch(matchCtx)
			if matchErr != nil {
				return nil, matchErr
			}
			query.Clauses = append(query.Clauses, clause)
		}
	case ctx.OC_Pattern() != nil:
		patterns, patternErr := b.bindPattern(ctx.OC_Pattern())
		if patternErr != nil {
			return nil, patternErr
		}
		match := &MatchClause{Span: b.span(ctx.OC_Pattern()), Patterns: patterns}
		if where := ctx.OC_Where(); where != nil {
			match.Where, patternErr = b.bindExpression(where.OC_Expression())
			if patternErr != nil {
				return nil, patternErr
			}
			match.Span.End = match.Where.Location().End
		}
		query = &QueryStatement{Span: match.Span, Clauses: []Clause{match}}
	default:
		return nil, b.unsupported(ctx, "EXISTS subquery", "no query or pattern body was recognized")
	}
	if err != nil {
		return nil, err
	}
	return &ExistsSubquery{Span: b.span(ctx), Subquery: query}, nil
}

func decodeStringLiteral(raw string) (string, error) {
	if len(raw) < 2 {
		return "", strconv.ErrSyntax
	}
	content := raw[1 : len(raw)-1]
	var result strings.Builder
	for len(content) > 0 {
		r, width := utf8.DecodeRuneInString(content)
		content = content[width:]
		if r != '\\' {
			result.WriteRune(r)
			continue
		}
		if len(content) == 0 {
			return "", strconv.ErrSyntax
		}
		escape, escapeWidth := utf8.DecodeRuneInString(content)
		content = content[escapeWidth:]
		switch escape {
		case '\\', '\'', '"':
			result.WriteRune(escape)
		case 'b', 'B':
			result.WriteByte('\b')
		case 'f', 'F':
			result.WriteByte('\f')
		case 'n', 'N':
			result.WriteByte('\n')
		case 'r', 'R':
			result.WriteByte('\r')
		case 't', 'T':
			result.WriteByte('\t')
		case 'u', 'U':
			digits := 4
			if len(content) >= 8 && allHex(content[:8]) {
				digits = 8
			}
			if len(content) < digits || !allHex(content[:digits]) {
				return "", strconv.ErrSyntax
			}
			value, err := strconv.ParseUint(content[:digits], 16, 32)
			if err != nil || !utf8.ValidRune(rune(value)) {
				return "", strconv.ErrSyntax
			}
			result.WriteRune(rune(value))
			content = content[digits:]
		default:
			return "", strconv.ErrSyntax
		}
	}
	return result.String(), nil
}

func allHex(value string) bool {
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
