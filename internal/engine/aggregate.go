package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func (e evaluator) aggregate(expression *cypher.FunctionInvocation, representative row) (any, error) {
	name := strings.ToLower(expression.Name.String())
	if name == "count" && expression.Star {
		return int64(len(e.group)), nil
	}
	if len(expression.Arguments) == 0 {
		return nil, evalError(expression, "%s expects an argument", name)
	}

	values := make([]any, 0, len(e.group))
	scalar := e
	scalar.group = nil
	for _, groupRow := range e.group {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		value, err := scalar.expression(expression.Arguments[0], groupRow)
		if err != nil {
			return nil, err
		}
		if value != nil {
			values = append(values, value)
		}
	}
	if expression.Distinct {
		distinct, err := distinctValues(e.ctx, values)
		if err != nil {
			return nil, err
		}
		values = distinct
	}

	switch name {
	case "count":
		return int64(len(values)), nil
	case "collect":
		return values, nil
	case "sum", "avg", "stdev", "stdevp":
		if len(values) == 0 {
			if name == "sum" {
				return int64(0), nil
			}
			return nil, nil
		}
		total := float64(0)
		allIntegers := true
		integerTotal := int64(0)
		for _, value := range values {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			number, integerValue, ok := number(value)
			if !ok {
				return nil, evalError(expression, "%s expects numeric values, got %T", name, value)
			}
			total += number
			if exact, ok := integer(value); ok && allIntegers {
				if exact > 0 && integerTotal > math.MaxInt64-exact || exact < 0 && integerTotal < math.MinInt64-exact {
					allIntegers = false
				} else {
					integerTotal += exact
				}
			} else if !integerValue {
				allIntegers = false
			}
		}
		if name == "sum" {
			if allIntegers {
				return integerTotal, nil
			}
			return total, nil
		}
		mean := total / float64(len(values))
		if name == "avg" {
			return mean, nil
		}
		if name == "stdev" && len(values) == 1 {
			return float64(0), nil
		}
		variance := float64(0)
		for _, value := range values {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			number, _, _ := number(value)
			difference := number - mean
			variance += difference * difference
		}
		divisor := float64(len(values))
		if name == "stdev" {
			divisor--
		}
		return math.Sqrt(variance / divisor), nil
	case "min", "max":
		if len(values) == 0 {
			return nil, nil
		}
		best := values[0]
		for _, value := range values[1:] {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			comparison, ok := compareValues(value, best)
			if !ok {
				return nil, evalError(expression, "%s cannot compare %T and %T", name, value, best)
			}
			if name == "min" && comparison < 0 || name == "max" && comparison > 0 {
				best = value
			}
		}
		return best, nil
	case "percentilecont", "percentiledisc":
		if len(expression.Arguments) != 2 {
			return nil, evalError(expression, "%s expects two arguments", name)
		}
		percentileValue, err := scalar.expression(expression.Arguments[1], representative)
		if err != nil {
			return nil, err
		}
		percentile, _, ok := number(percentileValue)
		if !ok || percentile < 0 || percentile > 1 {
			return nil, evalError(expression, "%s percentile must be between 0 and 1", name)
		}
		if len(values) == 0 {
			return nil, nil
		}
		numbers := make([]float64, len(values))
		for index, value := range values {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			numbers[index], _, ok = number(value)
			if !ok {
				return nil, evalError(expression, "%s expects numeric values", name)
			}
		}
		sort.Float64s(numbers)
		position := percentile * float64(len(numbers)-1)
		if name == "percentiledisc" {
			return numbers[int(math.Ceil(position))], nil
		}
		lower := int(math.Floor(position))
		upper := int(math.Ceil(position))
		if lower == upper {
			return numbers[lower], nil
		}
		fraction := position - float64(lower)
		return numbers[lower] + (numbers[upper]-numbers[lower])*fraction, nil
	default:
		return nil, fmt.Errorf("%w: %s", errUnsupportedAggregation, name)
	}
}

func distinctValues(ctx context.Context, values []any) ([]any, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]any, 0, len(values))
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := valueKey(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func valueKey(value any) string {
	canonical := canonicalKeyValue(value)
	encoded, err := json.Marshal(freezeValue(canonical))
	if err != nil {
		return fmt.Sprintf("%T:%#v", canonical, canonical)
	}
	return fmt.Sprintf("%T:%s", canonical, encoded)
}

func canonicalKeyValue(value any) any {
	switch value := value.(type) {
	case time.Time:
		if converted, ok := dateTimeFromLegacy(value); ok {
			return converted
		}
		return struct {
			LegacyTime string `json:"legacy_time"`
		}{legacyTimeFallbackKey(value)}
	case time.Duration:
		return legacyDuration(value)
	case []any:
		result := make([]any, len(value))
		for index := range value {
			result[index] = canonicalKeyValue(value[index])
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			result[key] = canonicalKeyValue(item)
		}
		return result
	case domain.Properties:
		result := make(domain.Properties, len(value))
		for key, item := range value {
			result[key] = canonicalKeyValue(item)
		}
		return result
	case temporal.Date, temporal.LocalTime, temporal.Time, temporal.LocalDateTime, temporal.DateTime, temporal.Duration:
		return value
	default:
		if items, ok := asList(value); ok {
			result := make([]any, len(items))
			for index := range items {
				result[index] = canonicalKeyValue(items[index])
			}
			return result
		}
		return value
	}
}
