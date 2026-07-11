package sandboxd

import (
	"log"
	"time"
)

// bootPhase logs how long each sandboxd boot stage takes, so the start→ready
// window can be attributed to a specific subsystem (proxy, FUSE, image unpack,
// rootfs assembly, agent launch, workload wait) rather than read as one number.
type bootPhase struct{ last time.Time }

// mark logs the elapsed time since the previous mark (or boot start) and resets
// the clock for the next stage.
func (b *bootPhase) mark(name string) {
	now := time.Now()
	log.Printf("sandboxd: boot phase %q took %s", name, now.Sub(b.last).Round(time.Millisecond))
	b.last = now
}
