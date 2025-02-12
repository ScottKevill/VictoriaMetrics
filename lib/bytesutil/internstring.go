package bytesutil

import (
	"flag"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
)

var internStringMaxLen = flag.Int("internStringMaxLen", 300, "The maximum length for strings to intern. Lower limit may save memory at the cost of higher CPU usage. "+
	"See https://en.wikipedia.org/wiki/String_interning")

// InternBytes interns b as a string
func InternBytes(b []byte) string {
	s := ToUnsafeString(b)
	return InternString(s)
}

// InternString returns interned s.
//
// This may be needed for reducing the amounts of allocated memory.
func InternString(s string) string {
	ct := fasttime.UnixTimestamp()
	if v, ok := internStringsMap.Load(s); ok {
		e := v.(*ismEntry)
		if atomic.LoadUint64(&e.lastAccessTime)+10 < ct {
			// Reduce the frequency of e.lastAccessTime update to once per 10 seconds
			// in order to improve the fast path speed on systems with many CPU cores.
			atomic.StoreUint64(&e.lastAccessTime, ct)
		}
		return e.s
	}
	// Make a new copy for s in order to remove references from possible bigger string s refers to.
	sCopy := strings.Clone(s)
	if len(sCopy) > *internStringMaxLen {
		// Do not intern long strings, since this may result in high memory usage
		// like in https://github.com/VictoriaMetrics/VictoriaMetrics/issues/3692
		return sCopy
	}

	e := &ismEntry{
		lastAccessTime: ct,
		s:              sCopy,
	}
	internStringsMap.Store(sCopy, e)

	if needCleanup(&internStringsMapLastCleanupTime, ct) {
		// Perform a global cleanup for internStringsMap by removing items, which weren't accessed during the last 5 minutes.
		m := &internStringsMap
		m.Range(func(k, v interface{}) bool {
			e := v.(*ismEntry)
			if atomic.LoadUint64(&e.lastAccessTime)+5*60 < ct {
				m.Delete(k)
			}
			return true
		})
	}

	return sCopy
}

type ismEntry struct {
	lastAccessTime uint64
	s              string
}

var (
	internStringsMap                sync.Map
	internStringsMapLastCleanupTime uint64
)
