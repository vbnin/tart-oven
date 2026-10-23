// Package m1cpu is a stand-in for github.com/shoenig/go-m1cpu, which
// gopsutil imports only to report the CPU frequency.
//
// The real package runs a cgo init() that reads IORegistry keys such as
// voltage-states1-sram and dereferences them without a NULL check. Those keys
// are absent on newer chips (for example the M5 Pro), so the whole process
// crashed with SIGSEGV before main() ran. Tart Oven never uses the frequency,
// so this package reports "not Apple Silicon" and does nothing at init.
package m1cpu

// IsAppleSilicon returns false so gopsutil falls back to its generic path.
func IsAppleSilicon() bool { return false }

func PCoreHz() uint64             { return 0 }
func ECoreHz() uint64             { return 0 }
func PCoreGHz() float64           { return 0 }
func ECoreGHz() float64           { return 0 }
func PCoreCount() int             { return 0 }
func ECoreCount() int             { return 0 }
func PCoreCache() (int, int, int) { return 0, 0, 0 }
func ECoreCache() (int, int, int) { return 0, 0, 0 }
func ModelName() string           { return "" }
