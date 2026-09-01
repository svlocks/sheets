// Package skills embeds the canonical agent skill so release binaries can
// print and install it without a repository checkout. skills/sheets/SKILL.md
// is the single source of truth; the embed keeps the binary in lockstep.
package skills

import _ "embed"

// SheetsSkill is the complete skills/sheets/SKILL.md document.
//
//go:embed sheets/SKILL.md
var SheetsSkill []byte
