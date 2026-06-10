package mcptest_test

import (
	"fmt"
	"runtime"
)

// sprintf is a thin alias so the fakeTB stub can format messages without each
// test file importing fmt.
func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// runtimeGoexit aliases runtime.Goexit so the fakeTB.Fatalf stub can abandon its
// goroutine the way testing.T.FailNow does.
func runtimeGoexit() {
	runtime.Goexit()
}
