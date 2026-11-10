package jxl

import (
	"fmt"
	"os"
	"time"
)

var debugOn = os.Getenv("JXLDEBUG") != ""

func dbg(format string, args ...any) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "[jxl] "+format+"\n", args...)
	}
}

// stage timing (enabled with JXLDEBUG)
type stageTimer struct{ t time.Time }

func startStage() stageTimer {
	if !debugOn {
		return stageTimer{}
	}
	return stageTimer{time.Now()}
}

func (s stageTimer) done(name string) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "[jxl] stage %-12s %v\n", name, time.Since(s.t))
	}
}
