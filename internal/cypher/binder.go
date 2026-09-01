package cypher

import (
	"fmt"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/svlocks/sheets/internal/cypher/parsergen"
)

// cstBinder is the only bridge from the generated official-grammar CST into
// sheets's execution-neutral AST. Keeping this conversion explicit makes
// capability decisions reviewable and prevents syntax from being validated by
// one parser and silently reinterpreted by another.
type cstBinder struct {
	mapper segmentMapper
	tokens antlr.TokenStream
}

func newBinder(index *sourceIndex, segment documentSegment) *cstBinder {
	return &cstBinder{mapper: segmentMapper{index: index, segment: segment}}
}

func (b *cstBinder) span(rule antlr.ParserRuleContext) Span { return b.mapper.ruleSpan(rule) }

func (b *cstBinder) unsupported(rule antlr.ParserRuleContext, feature, detail string) error {
	return &UnsupportedFeatureError{Span: b.span(rule), Feature: feature, Detail: detail}
}

func (b *cstBinder) bindCypher(ctx parsergen.IOC_CypherContext) (*QueryStatement, error) {
	if ctx == nil || ctx.OC_Statement() == nil || ctx.OC_Statement().OC_Query() == nil {
		return nil, &ParseError{Position: b.mapper.position(0), End: b.mapper.position(0), Message: "expected a Cypher statement"}
	}
	b.tokens = ctx.GetParser().GetTokenStream()
	query, err := b.bindQuery(ctx.OC_Statement().OC_Query())
	if err != nil {
		return nil, err
	}
	query.Explain = ctx.EXPLAIN() != nil
	query.Profile = ctx.PROFILE() != nil
	query.Span = b.span(ctx.OC_Statement().OC_Query())
	if modifier := ctx.EXPLAIN(); modifier != nil {
		query.Span.Start = b.mapper.tokenSpan(modifier.GetSymbol()).Start
	}
	if modifier := ctx.PROFILE(); modifier != nil {
		query.Span.Start = b.mapper.tokenSpan(modifier.GetSymbol()).Start
	}
	return query, nil
}

func (b *cstBinder) bindQuery(ctx parsergen.IOC_QueryContext) (*QueryStatement, error) {
	switch {
	case ctx.OC_RegularQuery() != nil:
		return b.bindRegularQuery(ctx.OC_RegularQuery())
	case ctx.OC_StandaloneCall() != nil:
		call, err := b.bindStandaloneCall(ctx.OC_StandaloneCall())
		if err != nil {
			return nil, err
		}
		return &QueryStatement{Span: b.span(ctx), Clauses: []Clause{call}}, nil
	case ctx.OC_SubqueryCall() != nil:
		call, err := b.bindSubqueryCall(ctx.OC_SubqueryCall())
		if err != nil {
			return nil, err
		}
		return &QueryStatement{Span: b.span(ctx), Clauses: []Clause{call}}, nil
	default:
		return nil, b.unsupported(ctx, "query form", "the generated CST did not select a bindable query alternative")
	}
}

func (b *cstBinder) bindRegularQuery(ctx parsergen.IOC_RegularQueryContext) (*QueryStatement, error) {
	query, err := b.bindSingleQuery(ctx.OC_SingleQuery())
	if err != nil {
		return nil, err
	}
	query.Span = b.span(ctx)
	for _, union := range ctx.AllOC_Union() {
		branch, branchErr := b.bindSingleQuery(union.OC_SingleQuery())
		if branchErr != nil {
			return nil, branchErr
		}
		if !queryConcludesWithReturn(branch) {
			return nil, b.unsupported(union, "updating UNION branch", "sheets requires every UNION branch to conclude with RETURN")
		}
		query.UnionBranches = append(query.UnionBranches, UnionBranch{
			Span:  b.span(union),
			All:   union.ALL() != nil,
			Query: branch,
		})
	}
	if len(query.UnionBranches) != 0 && !queryConcludesWithReturn(query) {
		return nil, b.unsupported(ctx.OC_SingleQuery(), "updating UNION branch", "sheets requires every UNION branch to conclude with RETURN")
	}
	return query, nil
}

func queryConcludesWithReturn(query *QueryStatement) bool {
	if query == nil || len(query.Clauses) == 0 {
		return false
	}
	projection, ok := query.Clauses[len(query.Clauses)-1].(*ProjectionClause)
	return ok && !projection.With
}

func (b *cstBinder) bindSingleQuery(ctx parsergen.IOC_SingleQueryContext) (*QueryStatement, error) {
	query := &QueryStatement{Span: b.span(ctx)}
	var err error
	if single := ctx.OC_SinglePartQuery(); single != nil {
		err = b.appendQueryChildren(query, single)
	} else if multi := ctx.OC_MultiPartQuery(); multi != nil {
		err = b.appendQueryChildren(query, multi)
	} else {
		err = b.unsupported(ctx, "single query form", "the generated CST contained no query body")
	}
	if err != nil {
		return nil, err
	}
	return query, nil
}

func (b *cstBinder) appendQueryChildren(query *QueryStatement, rule antlr.ParserRuleContext) error {
	for _, child := range rule.GetChildren() {
		var (
			clause Clause
			err    error
		)
		switch child := child.(type) {
		case parsergen.IOC_ReadingClauseContext:
			clause, err = b.bindReadingClause(child)
		case parsergen.IOC_UpdatingClauseContext:
			clause, err = b.bindUpdatingClause(child)
		case parsergen.IOC_WithContext:
			clause, err = b.bindProjection(child, true)
		case parsergen.IOC_ReturnContext:
			clause, err = b.bindProjection(child, false)
		case parsergen.IOC_SinglePartQueryContext:
			err = b.appendQueryChildren(query, child)
		default:
			if unhandled, ok := child.(antlr.ParserRuleContext); ok {
				return b.unsupported(unhandled, "query clause", "the generated CST contains an unhandled query child")
			}
			continue
		}
		if err != nil {
			return err
		}
		if clause != nil {
			query.Clauses = append(query.Clauses, clause)
		}
	}
	if len(query.Clauses) == 0 {
		return b.unsupported(rule, "empty query", "no executable clauses were bound")
	}
	return nil
}

func (b *cstBinder) bindReadingClause(ctx parsergen.IOC_ReadingClauseContext) (Clause, error) {
	switch {
	case ctx.OC_Match() != nil:
		return b.bindMatch(ctx.OC_Match())
	case ctx.OC_Unwind() != nil:
		return b.bindUnwind(ctx.OC_Unwind())
	case ctx.OC_InQueryCall() != nil:
		return b.bindInQueryCall(ctx.OC_InQueryCall())
	case ctx.OC_SubqueryCall() != nil:
		return b.bindSubqueryCall(ctx.OC_SubqueryCall())
	default:
		return nil, b.unsupported(ctx, "reading clause", "the generated CST selected no bindable clause")
	}
}

func (b *cstBinder) bindUpdatingClause(ctx parsergen.IOC_UpdatingClauseContext) (Clause, error) {
	switch {
	case ctx.OC_Create() != nil:
		patterns, err := b.bindPattern(ctx.OC_Create().OC_Pattern())
		if err != nil {
			return nil, err
		}
		return &CreateClause{Span: b.span(ctx.OC_Create()), Patterns: patterns}, nil
	case ctx.OC_Merge() != nil:
		return b.bindMerge(ctx.OC_Merge())
	case ctx.OC_Delete() != nil:
		return b.bindDelete(ctx.OC_Delete())
	case ctx.OC_Set() != nil:
		items, err := b.bindSetItems(ctx.OC_Set())
		if err != nil {
			return nil, err
		}
		return &SetClause{Span: b.span(ctx.OC_Set()), Items: items}, nil
	case ctx.OC_Remove() != nil:
		return b.bindRemove(ctx.OC_Remove())
	default:
		return nil, b.unsupported(ctx, "updating clause", "the generated CST selected no bindable clause")
	}
}

func (b *cstBinder) bindMatch(ctx parsergen.IOC_MatchContext) (Clause, error) {
	patterns, err := b.bindPattern(ctx.OC_Pattern())
	if err != nil {
		return nil, err
	}
	clause := &MatchClause{Span: b.span(ctx), Optional: ctx.OPTIONAL() != nil, Patterns: patterns}
	if where := ctx.OC_Where(); where != nil {
		clause.Where, err = b.bindExpression(where.OC_Expression())
	}
	return clause, err
}

func (b *cstBinder) bindUnwind(ctx parsergen.IOC_UnwindContext) (Clause, error) {
	expression, err := b.bindExpression(ctx.OC_Expression())
	if err != nil {
		return nil, err
	}
	return &UnwindClause{Span: b.span(ctx), Expression: expression, Alias: b.bindVariableIdentifier(ctx.OC_Variable())}, nil
}

type projectionRule interface {
	antlr.ParserRuleContext
	OC_ProjectionBody() parsergen.IOC_ProjectionBodyContext
}

func (b *cstBinder) bindProjection(ctx projectionRule, with bool) (Clause, error) {
	body := ctx.OC_ProjectionBody()
	clause := &ProjectionClause{Span: b.span(ctx), With: with, Distinct: body.DISTINCT() != nil}
	items := body.OC_ProjectionItems()
	if star := directToken(items, "*"); star != nil {
		clause.Items = append(clause.Items, ProjectionItem{Span: b.mapper.tokenSpan(star.GetSymbol()), Star: true})
	}
	for _, itemCtx := range items.AllOC_ProjectionItem() {
		expression, err := b.bindExpression(itemCtx.OC_Expression())
		if err != nil {
			return nil, err
		}
		item := ProjectionItem{Span: b.span(itemCtx), Expression: expression}
		if alias := itemCtx.OC_Variable(); alias != nil {
			item.Alias = b.bindVariableIdentifier(alias)
		}
		clause.Items = append(clause.Items, item)
	}
	if order := body.OC_Order(); order != nil {
		for _, itemCtx := range order.AllOC_SortItem() {
			expression, err := b.bindExpression(itemCtx.OC_Expression())
			if err != nil {
				return nil, err
			}
			clause.OrderBy = append(clause.OrderBy, SortItem{
				Span:       b.span(itemCtx),
				Expression: expression,
				Descending: itemCtx.DESC() != nil || itemCtx.DESCENDING() != nil,
			})
		}
	}
	var err error
	if skip := body.OC_Skip(); skip != nil {
		clause.Skip, err = b.bindExpression(skip.OC_Expression())
		if err != nil {
			return nil, err
		}
	}
	if limit := body.OC_Limit(); limit != nil {
		clause.Limit, err = b.bindExpression(limit.OC_Expression())
		if err != nil {
			return nil, err
		}
	}
	if withCtx, ok := ctx.(parsergen.IOC_WithContext); ok && withCtx.OC_Where() != nil {
		clause.Where, err = b.bindExpression(withCtx.OC_Where().OC_Expression())
	}
	return clause, err
}

func (b *cstBinder) bindMerge(ctx parsergen.IOC_MergeContext) (Clause, error) {
	pattern, err := b.bindPatternPart(ctx.OC_PatternPart())
	if err != nil {
		return nil, err
	}
	clause := &MergeClause{Span: b.span(ctx), Pattern: pattern}
	for _, actionCtx := range ctx.AllOC_MergeAction() {
		items, itemErr := b.bindSetItems(actionCtx.OC_Set())
		if itemErr != nil {
			return nil, itemErr
		}
		kind := OnMatch
		if actionCtx.CREATE() != nil {
			kind = OnCreate
		}
		clause.Actions = append(clause.Actions, MergeAction{Span: b.span(actionCtx), Kind: kind, Set: items})
	}
	return clause, nil
}

func (b *cstBinder) bindSetItems(ctx parsergen.IOC_SetContext) ([]SetItem, error) {
	items := make([]SetItem, 0, len(ctx.AllOC_SetItem()))
	for _, itemCtx := range ctx.AllOC_SetItem() {
		item := SetItem{Span: b.span(itemCtx)}
		var err error
		switch {
		case itemCtx.OC_PropertyExpression() != nil:
			item.Target, err = b.bindPropertyExpression(itemCtx.OC_PropertyExpression())
			item.Operator = "="
		case itemCtx.OC_NodeLabels() != nil:
			variable := b.bindVariableExpression(itemCtx.OC_Variable())
			item.Target = variable
			item.Labels = b.bindNodeLabels(itemCtx.OC_NodeLabels())
		case itemCtx.OC_Variable() != nil:
			item.Target = b.bindVariableExpression(itemCtx.OC_Variable())
			item.Operator = b.operatorBetween(itemCtx.OC_Variable(), itemCtx.OC_Expression())
		default:
			return nil, b.unsupported(itemCtx, "SET item", "the assignment target cannot be represented")
		}
		if err != nil {
			return nil, err
		}
		if expression := itemCtx.OC_Expression(); expression != nil {
			item.Value, err = b.bindExpression(expression)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, item)
	}
	return items, nil
}

func (b *cstBinder) bindDelete(ctx parsergen.IOC_DeleteContext) (Clause, error) {
	clause := &DeleteClause{Span: b.span(ctx), Detach: ctx.DETACH() != nil}
	for _, expressionCtx := range ctx.AllOC_Expression() {
		expression, err := b.bindExpression(expressionCtx)
		if err != nil {
			return nil, err
		}
		clause.Expressions = append(clause.Expressions, expression)
	}
	return clause, nil
}

func (b *cstBinder) bindRemove(ctx parsergen.IOC_RemoveContext) (Clause, error) {
	clause := &RemoveClause{Span: b.span(ctx)}
	for _, itemCtx := range ctx.AllOC_RemoveItem() {
		item := RemoveItem{Span: b.span(itemCtx)}
		if property := itemCtx.OC_PropertyExpression(); property != nil {
			target, err := b.bindPropertyExpression(property)
			if err != nil {
				return nil, err
			}
			item.Target = target
		} else {
			item.Target = b.bindVariableExpression(itemCtx.OC_Variable())
			item.Labels = b.bindNodeLabels(itemCtx.OC_NodeLabels())
		}
		clause.Items = append(clause.Items, item)
	}
	return clause, nil
}

func (b *cstBinder) bindInQueryCall(ctx parsergen.IOC_InQueryCallContext) (Clause, error) {
	return b.bindProcedureCall(ctx, ctx.OC_ExplicitProcedureInvocation(), nil, ctx.OC_YieldItems(), directToken(ctx, "*") != nil)
}

func (b *cstBinder) bindStandaloneCall(ctx parsergen.IOC_StandaloneCallContext) (Clause, error) {
	return b.bindProcedureCall(ctx, ctx.OC_ExplicitProcedureInvocation(), ctx.OC_ImplicitProcedureInvocation(), ctx.OC_YieldItems(), directToken(ctx, "*") != nil)
}

func (b *cstBinder) bindProcedureCall(
	rule antlr.ParserRuleContext,
	explicit parsergen.IOC_ExplicitProcedureInvocationContext,
	implicit parsergen.IOC_ImplicitProcedureInvocationContext,
	yieldCtx parsergen.IOC_YieldItemsContext,
	yieldStar bool,
) (Clause, error) {
	clause := &CallClause{Span: b.span(rule)}
	if explicit != nil {
		clause.Procedure = b.bindProcedureName(explicit.OC_ProcedureName())
		for _, argumentCtx := range explicit.AllOC_Expression() {
			argument, err := b.bindExpression(argumentCtx)
			if err != nil {
				return nil, err
			}
			clause.Arguments = append(clause.Arguments, argument)
		}
	} else if implicit != nil {
		clause.Procedure = b.bindProcedureName(implicit.OC_ProcedureName())
	} else {
		return nil, b.unsupported(rule, "procedure invocation", "no procedure name was recognized")
	}
	if !supportedProcedure(clause.Procedure.String()) {
		return nil, b.unsupported(rule, "procedure invocation", "procedure "+clause.Procedure.String()+" is not in sheets's supported procedure catalog")
	}
	if yieldStar {
		star := directToken(rule, "*")
		clause.Yield = append(clause.Yield, YieldItem{Span: b.mapper.tokenSpan(star.GetSymbol()), Star: true})
	}
	if yieldCtx != nil {
		for _, itemCtx := range yieldCtx.AllOC_YieldItem() {
			item := YieldItem{Span: b.span(itemCtx)}
			if field := itemCtx.OC_ProcedureResultField(); field != nil {
				item.Name = b.bindIdentifier(field.OC_SymbolicName())
				item.Alias = b.bindVariableIdentifier(itemCtx.OC_Variable())
			} else {
				item.Name = b.bindVariableIdentifier(itemCtx.OC_Variable())
			}
			clause.Yield = append(clause.Yield, item)
		}
		if where := yieldCtx.OC_Where(); where != nil {
			var err error
			clause.YieldWhere, err = b.bindExpression(where.OC_Expression())
			if err != nil {
				return nil, err
			}
		}
	}
	return clause, nil
}

func supportedProcedure(name string) bool {
	switch strings.ToLower(name) {
	case "db.labels", "db.relationshiptypes", "db.propertykeys", "sheets.nodes", "sheets.edges", "sheets.revisions":
		return true
	default:
		return false
	}
}

func (b *cstBinder) bindSubqueryCall(ctx parsergen.IOC_SubqueryCallContext) (Clause, error) {
	subquery, err := b.bindRegularQuery(ctx.OC_RegularQuery())
	if err != nil {
		return nil, err
	}
	return &CallClause{Span: b.span(ctx), Subquery: subquery}, nil
}

func (b *cstBinder) bindProcedureName(ctx parsergen.IOC_ProcedureNameContext) QualifiedName {
	parts := make([]Identifier, 0, len(ctx.OC_Namespace().AllOC_SymbolicName())+1)
	for _, part := range ctx.OC_Namespace().AllOC_SymbolicName() {
		parts = append(parts, b.bindIdentifier(part))
	}
	parts = append(parts, b.bindIdentifier(ctx.OC_SymbolicName()))
	return QualifiedName{Span: b.span(ctx), Parts: parts}
}

func (b *cstBinder) bindFunctionName(ctx parsergen.IOC_FunctionNameContext) QualifiedName {
	parts := make([]Identifier, 0, len(ctx.OC_Namespace().AllOC_SymbolicName())+1)
	for _, part := range ctx.OC_Namespace().AllOC_SymbolicName() {
		parts = append(parts, b.bindIdentifier(part))
	}
	parts = append(parts, b.bindIdentifier(ctx.OC_SymbolicName()))
	return QualifiedName{Span: b.span(ctx), Parts: parts}
}

func (b *cstBinder) bindVariableIdentifier(ctx parsergen.IOC_VariableContext) Identifier {
	return b.bindIdentifier(ctx.OC_SymbolicName())
}

func (b *cstBinder) bindIdentifier(rule antlr.ParserRuleContext) Identifier {
	raw := rule.GetText()
	identifier := Identifier{Span: b.span(rule), Name: raw}
	if strings.HasPrefix(raw, "`") {
		identifier.BacktickQuoted = true
		identifier.Name = decodeEscapedIdentifier(raw)
	}
	return identifier
}

func decodeEscapedIdentifier(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	return strings.ReplaceAll(raw[1:len(raw)-1], "``", "`")
}

func directToken(rule antlr.ParserRuleContext, text string) antlr.TerminalNode {
	for _, child := range rule.GetChildren() {
		terminal, ok := child.(antlr.TerminalNode)
		if ok && terminal.GetText() == text {
			return terminal
		}
	}
	return nil
}

func (b *cstBinder) operatorBetween(left, right antlr.ParserRuleContext) string {
	if left == nil || right == nil || b.tokens == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	for index := left.GetStop().GetTokenIndex() + 1; index < right.GetStart().GetTokenIndex(); index++ {
		token := b.tokens.Get(index)
		if token.GetTokenType() == parsergen.CypherParserSP || token.GetTokenType() == antlr.TokenEOF {
			continue
		}
		parts = append(parts, strings.ToUpper(token.GetText()))
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, " ")
}

func binderInvariant(rule antlr.ParserRuleContext, format string, arguments ...any) error {
	return fmt.Errorf("cypher binder invariant at %s: %s", rule.GetText(), fmt.Sprintf(format, arguments...))
}
