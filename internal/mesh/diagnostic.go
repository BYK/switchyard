package mesh

import (
	"fmt"
	"os"
	"time"
)

// Diagnostics contain only fixed phase/result labels and timings. In particular,
// never log commands, paths, cloud responses or the invocation environment.
func debugEvent(phase, result string, started time.Time) {
	if os.Getenv("MESH_DEBUG") != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "mesh-go: debug phase=%s result=%s elapsed_ms=%d\n",
		phase, result, time.Since(started).Milliseconds())
}
