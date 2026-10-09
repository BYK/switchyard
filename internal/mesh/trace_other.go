//go:build !linux || !amd64

package mesh

import "errors"

func traceCommand(_, _ string, _ []string) int {
	return fail(errors.New("exec tracing requires Linux x86-64"))
}

func traceShell(_ string, _ []string) int {
	return fail(errors.New("exec tracing requires Linux x86-64"))
}
