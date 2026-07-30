//go:build linux

package main

/*
#include <signal.h>
#include <stddef.h>

// sigFlags returns a signal's current handler flags without modifying them.
static unsigned long sigFlags(int sig) {
	struct sigaction sa;
	if (sigaction(sig, NULL, &sa) != 0) return (unsigned long)-1;
	return (unsigned long)sa.sa_flags;
}

// repairOnStack adds SA_ONSTACK to a handler that lacks it, keeping the handler
// function itself untouched. Returns 1 if it changed something, 0 if nothing needed
// doing, -1 on error.
static int repairOnStack(int sig) {
	struct sigaction sa;
	if (sigaction(sig, NULL, &sa) != 0) return -1;
	if (sa.sa_flags & SA_ONSTACK) return 0;
	sa.sa_flags |= SA_ONSTACK;
	if (sigaction(sig, &sa, NULL) != 0) return -1;
	return 1;
}
*/
import "C"

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Signal-handler diagnostics for the SA_ONSTACK crash.
//
// The failure is:
//
//	signal 11 received but handler not on signal stack
//	fatal error: non-Go code set up signal handler without SA_ONSTACK flag
//
// Go installs handlers for the synchronous signals with SA_ONSTACK, so they run on a
// dedicated stack. If native code re-installs a handler for one of those signals
// *without* the flag, the next such signal runs on the current stack and the runtime
// aborts. Nothing in the traceback identifies which library did it — the goroutine shown
// is just whichever one happened to be in a cgo call.
//
// So this measures it directly instead: sample the flags at points during startup and
// report the first moment one loses SA_ONSTACK, along with the shared objects that
// appeared since the previous sample. That names the library rather than inferring it.
//
// Opt-in via LOCALCHAT_SIGPROBE=1 so it costs nothing in normal use.
// LOCALCHAT_SIGFIX=1 additionally re-adds the flag, which is the standard remedy when the
// offending library cannot be changed — it keeps that library's handler and only corrects
// how it is entered.

// probedSignals are the ones Go handles on its own signal stack, so all four are
// candidates for being clobbered.
var probedSignals = []struct {
	num  int
	name string
}{
	{4, "SIGILL"}, {7, "SIGBUS"}, {8, "SIGFPE"}, {11, "SIGSEGV"},
}

var (
	sigProbeOn = os.Getenv("LOCALCHAT_SIGPROBE") == "1"
	sigFixOn   = os.Getenv("LOCALCHAT_SIGFIX") == "1"
	lastLibs   map[string]bool
)

// probeSignals reports handler flags at a labelled point in startup.
//
// Writes to stderr rather than slog: this has to survive a fatal error moments later,
// and stderr is unbuffered and always present.
func probeSignals(label string) {
	if !sigProbeOn {
		return
	}
	var bad []string
	var parts []string
	for _, s := range probedSignals {
		flags := uint64(C.sigFlags(C.int(s.num)))
		onStack := flags != ^uint64(0) && flags&0x08000000 != 0 // SA_ONSTACK
		parts = append(parts, fmt.Sprintf("%s=%s", s.name, map[bool]string{true: "ok", false: "NO_ONSTACK"}[onStack]))
		if !onStack {
			bad = append(bad, s.name)
		}
	}
	fmt.Fprintf(os.Stderr, "[sigprobe] %-34s %s\n", label, strings.Join(parts, " "))

	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "[sigprobe]   >>> %s lost SA_ONSTACK by this point\n",
			strings.Join(bad, ", "))
		for _, lib := range newLibraries() {
			fmt.Fprintf(os.Stderr, "[sigprobe]   newly loaded: %s\n", lib)
		}
		if sigFixOn {
			for _, s := range probedSignals {
				if C.repairOnStack(C.int(s.num)) == 1 {
					fmt.Fprintf(os.Stderr, "[sigprobe]   re-added SA_ONSTACK to %s\n", s.name)
				}
			}
		}
		return
	}
	// Track libraries even while healthy, so the first bad sample can report only what
	// arrived since the last good one.
	newLibraries()
}

// newLibraries returns shared objects mapped since the previous call.
//
// The library that clobbered a handler almost certainly did it in its initialiser, so it
// will be among those loaded between the last clean sample and the first dirty one.
func newLibraries() []string {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, "/")
		if i < 0 || !strings.Contains(line, ".so") {
			continue
		}
		seen[line[i:]] = true
	}

	var added []string
	if lastLibs != nil {
		for lib := range seen {
			if !lastLibs[lib] {
				added = append(added, lib)
			}
		}
		sort.Strings(added)
	}
	lastLibs = seen
	return added
}
