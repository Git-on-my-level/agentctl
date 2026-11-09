package store

import (
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"go.etcd.io/bbolt"
)

type maintenanceCompletion struct {
	version     int
	path, links string
	nextBefore  time.Time
	nextCheckAt time.Time
}

func maintenanceLinksKey(links []ids.ExecutionID) string {
	values := make([]string, len(links))
	for i, id := range links {
		values[i] = id.String()
	}
	sort.Strings(values)
	return strings.Join(values, "\x00")
}

// One short read skips a previously completed generation. Time advancing past
// the next terminal's cutoff, subscription expiry, or changed external
// protections forces a scan.
// A pass that deleted records retains its scanned (pre-commit) generation, so
// its own commit guarantees another pass can drain the remaining backlog.
func (r *cleanupReader) maintenanceUnchanged(done maintenanceCompletion, before time.Time, links []ids.ExecutionID) (bool, error) {
	if (!done.nextCheckAt.IsZero() && !r.now.Before(done.nextCheckAt)) || done.links != maintenanceLinksKey(links) || (!done.nextBefore.IsZero() && before.After(done.nextBefore)) {
		return false, nil
	}
	unchanged := false
	err := r.view(func(tx *bbolt.Tx) error { unchanged = tx.ID() == done.version; return nil })
	return unchanged, err
}
