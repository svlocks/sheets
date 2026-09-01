package temporal

import "errors"

// ErrInvalid reports a malformed or internally inconsistent temporal value.
var ErrInvalid = errors.New("invalid temporal value")

// ErrOverflow reports a result outside the durable representation's bounds.
var ErrOverflow = errors.New("temporal value overflow")

const (
	// MinYear and MaxYear match the practical range used by java.time, whose
	// temporal model informed the openCypher proposal.  Arithmetic and parsing
	// reject values outside this documented, cross-platform range.
	MinYear int64 = -999_999_999
	MaxYear int64 = 999_999_999

	nanosecondsPerSecond int64 = 1_000_000_000
	secondsPerDay        int64 = 86_400
	nanosecondsPerDay    int64 = secondsPerDay * nanosecondsPerSecond
)

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func floorDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor < 0 {
		quotient--
	}
	return quotient
}

func floorMod(value, divisor int64) int64 {
	result := value % divisor
	if result < 0 {
		result += divisor
	}
	return result
}

func checkedAdd(left, right int64) (int64, error) {
	result := left + right
	if right > 0 && result < left || right < 0 && result > left {
		return 0, ErrOverflow
	}
	return result, nil
}

func checkedSub(left, right int64) (int64, error) {
	if right == -1<<63 {
		if left >= 0 {
			return 0, ErrOverflow
		}
		return left - right, nil
	}
	return checkedAdd(left, -right)
}

func checkedMul(left, right int64) (int64, error) {
	if left == 0 || right == 0 {
		return 0, nil
	}
	if left == -1 && right == -1<<63 || right == -1 && left == -1<<63 {
		return 0, ErrOverflow
	}
	result := left * right
	if result/right != left {
		return 0, ErrOverflow
	}
	return result, nil
}
