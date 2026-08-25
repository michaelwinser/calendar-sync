package calendar

import (
	"context"
	"sync"
	"time"
)

// DeleteEventsOptions tunes bulk deletion.
type DeleteEventsOptions struct {
	Concurrency int           // max deletes in flight (default 6)
	RateLimit   float64       // max delete starts per second (default 8)
	Deadline    time.Duration // wall-clock budget for the whole call; 0 = none
}

// DeleteResult reports a bulk-deletion outcome. Deleted + Failed + len(Unprocessed)
// always equals the number of input IDs. Unprocessed lists IDs not attempted (the
// deadline/cancellation hit first), so the caller can re-queue them; deletion is
// idempotent, so re-queuing is safe.
type DeleteResult struct {
	Deleted     int
	Failed      int
	Unprocessed []string
	SampleError string // message from one failed delete, for surfacing to the user
}

// DeleteEvents deletes many events with bounded concurrency and a global
// start-rate limit, returning partial results if the deadline hits rather than
// blocking open-endedly. Each delete is idempotent (already-gone counts as deleted)
// and retries only transient errors — see DeleteEvent and Client.do — so a
// permanent failure (e.g. a read-only calendar) is counted, not retried forever.
func (c *Client) DeleteEvents(ctx context.Context, token, calendarID string, eventIDs []string, opts DeleteEventsOptions) DeleteResult {
	if len(eventIDs) == 0 {
		return DeleteResult{}
	}

	conc := opts.Concurrency
	if conc <= 0 {
		conc = 6
	}
	if conc > len(eventIDs) {
		conc = len(eventIDs)
	}
	rps := opts.RateLimit
	if rps <= 0 {
		rps = 8
	}
	if opts.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Deadline)
		defer cancel()
	}

	// One shared ticker gates delete *starts* across all workers to ~rps/sec.
	interval := time.Duration(float64(time.Second) / rps)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	const (
		stUnprocessed = 0
		stDeleted     = 1
		stFailed      = 2
	)
	// Each index is written by exactly one worker (it claims that index from idxCh),
	// and read only after wg.Wait(), so no synchronization is needed per-element.
	status := make([]byte, len(eventIDs))

	var (
		errOnce   sync.Once
		sampleErr string
	)

	idxCh := make(chan int)
	go func() {
		defer close(idxCh)
		for i := range eventIDs {
			select {
			case <-ctx.Done():
				return
			case idxCh <- i:
			}
		}
	}()

	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idxCh {
				select {
				case <-ctx.Done():
					return // leave status[i] = unprocessed
				case <-ticker.C:
				}
				err := c.DeleteEvent(ctx, token, calendarID, eventIDs[i])
				switch {
				case err == nil:
					status[i] = stDeleted
				case ctx.Err() != nil:
					// Deadline/cancel struck during this delete — leave it unprocessed
					// (retryable) rather than reporting a failure we're unsure of.
					return
				default:
					status[i] = stFailed
					errOnce.Do(func() { sampleErr = err.Error() })
				}
			}
		}()
	}
	wg.Wait()

	res := DeleteResult{SampleError: sampleErr}
	for i, s := range status {
		switch s {
		case stDeleted:
			res.Deleted++
		case stFailed:
			res.Failed++
		default:
			res.Unprocessed = append(res.Unprocessed, eventIDs[i])
		}
	}
	return res
}
