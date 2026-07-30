// Command dbcheck isolates the DuckDB open path from the GUI.
//
// It answers two questions the app cannot: does opening the database crash on its own,
// and does DuckDB's native initialisation replace the SIGSEGV handler with one that
// lacks SA_ONSTACK. The second is the actual mechanism behind
//
//	fatal error: non-Go code set up signal handler without SA_ONSTACK flag
//
// Go installs its handlers with SA_ONSTACK so signals land on a dedicated stack. If a C
// library re-installs a handler for the same signal without that flag, the next signal
// runs on the wrong stack and the Go runtime aborts. Printing the flag either side of
// the open says whether DuckDB is the library doing it.
package main

/*
#include <signal.h>
#include <stddef.h>

// segvFlags returns the current SIGSEGV handler's flags without changing them.
static unsigned long segvFlags(void) {
	struct sigaction sa;
	if (sigaction(SIGSEGV, NULL, &sa) != 0) {
		return (unsigned long)-1;
	}
	return (unsigned long)sa.sa_flags;
}

static int onStack(unsigned long flags) { return (flags & SA_ONSTACK) ? 1 : 0; }
*/
import "C"

import (
	"fmt"
	"os"

	// The same blank import the app uses — the store package deliberately does not
	// carry the driver, so the probe must register it exactly as main.go does.
	_ "github.com/duckdb/duckdb-go/v2"
	"simple-cot-chat/store"
)

func report(when string) {
	flags := C.segvFlags()
	fmt.Printf("%-22s SIGSEGV sa_flags=0x%x  SA_ONSTACK=%v\n",
		when, uint64(flags), C.onStack(flags) == 1)
}

func main() {
	report("before opening DB:")

	s, err := store.Open()
	if err != nil {
		report("after failed open:")
		fmt.Fprintln(os.Stderr, "open failed:", err)
		os.Exit(1)
	}
	report("after opening DB:")

	// Exercise a real query, in case initialisation is lazy.
	if st, err := s.MemoryStats(); err != nil {
		fmt.Fprintln(os.Stderr, "query failed:", err)
	} else {
		fmt.Printf("query ok: %d sources, %d chunks\n", st.Sources, st.Chunks)
	}
	report("after a query:")

	if err := s.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close failed:", err)
	}
	report("after close:")
	fmt.Println("\nno crash: opening DuckDB on its own is fine")
}
