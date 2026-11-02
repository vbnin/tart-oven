package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// VM name templates. A template is a VM name that may contain two variables:
//
//	$RAND8    8 random capital characters (0-9, A-F), e.g. 3F9A01BC
//	$AUTONUM  the next free number: 1, 2, 3, ...
//
// If the resulting name is already taken, "-1", "-2", ... is appended.
const (
	varRand8     = "$RAND8"
	varAutoNum   = "$AUTONUM"
	maxVMNameLen = 100
)

// expandNameTemplate substitutes the variables in tpl.
func expandNameTemplate(tpl string, num int, rand8 string) string {
	var b strings.Builder
	for i := 0; i < len(tpl); {
		switch {
		case strings.HasPrefix(tpl[i:], varRand8):
			b.WriteString(rand8)
			i += len(varRand8)
		case strings.HasPrefix(tpl[i:], varAutoNum):
			b.WriteString(strconv.Itoa(num))
			i += len(varAutoNum)
		default:
			b.WriteByte(tpl[i])
			i++
		}
	}
	return b.String()
}

// hasNameVariable reports whether tpl uses $RAND8 or $AUTONUM.
func hasNameVariable(tpl string) bool {
	return strings.Contains(tpl, varRand8) || strings.Contains(tpl, varAutoNum)
}

// validateNameTemplate rejects unknown $VARIABLES (a typo like $AUTONUMBER
// would otherwise end up in the VM name) and templates that could never make a
// valid name.
func validateNameTemplate(tpl string) error {
	if strings.TrimSpace(tpl) == "" {
		tpl = varRand8 // a blank template means $RAND8
	}
	isTokenChar := func(c byte) bool { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' }
	for i := 0; i < len(tpl); i++ {
		if tpl[i] != '$' {
			continue
		}
		known := 0
		for _, v := range []string{varRand8, varAutoNum} {
			if strings.HasPrefix(tpl[i:], v) {
				known = len(v)
			}
		}
		if known > 0 {
			next := i + known
			// "$AUTONUMBER" is a typo for $AUTONUM, not "$AUTONUM" + "BER".
			if next >= len(tpl) || tpl[next] < 'A' || tpl[next] > 'Z' {
				i = next - 1
				continue
			}
		} else if i+1 >= len(tpl) || tpl[i+1] < 'A' || tpl[i+1] > 'Z' {
			continue // a literal "$" such as "cost$5"
		}
		j := i + 1
		for j < len(tpl) && isTokenChar(tpl[j]) {
			j++
		}
		return fmt.Errorf("unknown variable %s (use %s or %s)", tpl[i:j], varRand8, varAutoNum)
	}
	// A worst-case expansion must still be a usable name.
	return validateVMName(expandNameTemplate(tpl, 99999, "FFFFFFFF") + "-99")
}

// validateVMName checks a final VM name. Tart treats names with "/" or ":" as
// remote images, and a leading "-" would be read as a command-line option.
func validateVMName(name string) error {
	switch {
	case name == "":
		return errors.New("VM name is empty")
	case len(name) > maxVMNameLen:
		return fmt.Errorf("VM name is longer than %d characters", maxVMNameLen)
	case strings.ContainsAny(name, "/:\\"):
		return errors.New(`VM name can't contain "/", ":" or "\"`)
	case strings.HasPrefix(name, "-") || strings.HasPrefix(name, "."):
		return errors.New(`VM name can't start with "-" or "."`)
	case name != strings.TrimSpace(name):
		return errors.New("VM name can't start or end with a space")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("VM name contains control characters")
		}
	}
	return nil
}

// vmNamer hands out the names for one create batch.
type vmNamer struct {
	template string
	lastNum  int               // last $AUTONUM handed out in this batch
	taken    func(string) bool // names already in use
	rand8    func() string     // overridden in tests
}

func newVMNamer(template string, taken func(string) bool) *vmNamer {
	if strings.TrimSpace(template) == "" {
		template = varRand8
	}
	return &vmNamer{template: strings.TrimSpace(template), taken: taken, rand8: shortID}
}

// next returns the next free name. With $AUTONUM it is the lowest number above
// the previous one whose name is free; otherwise a taken name gets a numeric
// suffix.
func (n *vmNamer) next() (string, error) {
	rand8 := n.rand8()
	if strings.Contains(n.template, varAutoNum) {
		for num := n.lastNum + 1; num <= n.lastNum+100000; num++ {
			name := expandNameTemplate(n.template, num, rand8)
			if n.taken(name) {
				continue
			}
			if err := validateVMName(name); err != nil {
				return "", err
			}
			n.lastNum = num
			return name, nil
		}
		return "", errors.New("no free number left for this name template")
	}
	base := expandNameTemplate(n.template, 0, rand8)
	if err := validateVMName(base); err != nil {
		return "", err
	}
	name := base
	for suffix := 1; n.taken(name); suffix++ {
		if suffix > 100000 {
			return "", errors.New("no free name left for this name template")
		}
		name = base + "-" + strconv.Itoa(suffix)
	}
	return name, validateVMName(name)
}

// allocVMName picks the next name for a batch and reserves it until the batch
// is done with it, so two batches running at once can't pick the same one.
// "Taken" means a known VM, a VM folder already on disk (tart may have created
// one that hasn't been noticed yet), a running task's target, or a reservation.
func (m *Manager) allocVMName(n *vmNamer) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n.taken = func(name string) bool {
		if _, ok := m.vms[name]; ok {
			return true
		}
		if _, ok := m.reservedNames[name]; ok {
			return true
		}
		for _, t := range m.tasks {
			if t.Status == "running" && t.Target == name {
				return true
			}
		}
		_, err := os.Stat(filepath.Join(m.cfg.VMStoragePath, "vms", name))
		return err == nil
	}
	name, err := n.next()
	if err != nil {
		return "", err
	}
	if m.reservedNames == nil {
		m.reservedNames = map[string]struct{}{}
	}
	m.reservedNames[name] = struct{}{}
	return name, nil
}

func (m *Manager) releaseVMName(name string) {
	m.mu.Lock()
	delete(m.reservedNames, name)
	m.mu.Unlock()
}
