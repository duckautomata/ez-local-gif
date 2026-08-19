package discordlint

import (
	"fmt"
	"strings"
)

// checkList accumulates rule outcomes in evaluation order.
type checkList []Check

func (c *checkList) add(rule string, level Level, ok, fixed bool, detail string) {
	*c = append(*c, Check{Rule: rule, Level: level, OK: ok, Fixed: fixed, Detail: detail})
}

// pass records a passing check.
func (c *checkList) pass(rule string, level Level, detail string) {
	c.add(rule, level, true, false, detail)
}

// fixed records a check that was violated but corrected by the fixer.
func (c *checkList) fixed(rule string, level Level, detail string) {
	c.add(rule, level, true, true, detail)
}

// fail records a violated (and uncorrected) check.
func (c *checkList) fail(rule string, level Level, detail string) {
	c.add(rule, level, false, false, detail)
}

// outcome records a violation as fixed when fixed is true and as failed
// otherwise; passing rules should use pass.
func (c *checkList) outcome(rule string, level Level, fixed bool, detail string) {
	c.add(rule, level, fixed, fixed, detail)
}

// allOK reports whether no LevelError check failed.
func (c checkList) allOK() bool {
	for _, ch := range c {
		if !ch.OK && ch.Level == LevelError {
			return false
		}
	}
	return true
}

// frameList renders frame numbers for details, eliding long lists:
// "frame 3", "frames 0, 3, 7" or "frames 0, 1, 2, 3, 4 and 12 more".
func frameList(idx []int) string {
	const show = 5
	parts := make([]string, 0, min(len(idx), show))
	for _, i := range idx[:min(len(idx), show)] {
		parts = append(parts, fmt.Sprint(i))
	}
	s := strings.Join(parts, ", ")
	if len(idx) > show {
		s += fmt.Sprintf(" and %d more", len(idx)-show)
	}
	if len(idx) == 1 {
		return "frame " + s
	}
	return "frames " + s
}

// plural returns "n word" / "n words".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// plays words a number of animation plays for loop-count details:
// "loops forever" (0), "plays once", "plays n times".
func plays(n int) string {
	switch n {
	case 0:
		return "loops forever"
	case 1:
		return "plays once"
	}
	return fmt.Sprintf("plays %d times", n)
}
