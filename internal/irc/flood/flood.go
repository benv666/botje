// Package flood is the outbound rate limiter and send queue, the Go
// counterpart of the Perl IRC.pm flood control: a high-priority queue
// (PONG and friends) that bypasses everything, normal traffic bucketed
// per channel and drained round-robin so one busy channel cannot starve
// the others, 1 event per second with waits capped at 3s, and lines
// over 80 bytes counting as multiple events. One fix vs Perl: the
// round-robin rotation is per queue, not shared program-wide.
//
// The queue does not do I/O; the connection layer asks Next for the
// next sendable line and sleeps for the returned wait.
package flood

import (
	"regexp"
	"time"
)

// sentinel bucket for non-PRIVMSG lines (the Perl C_IRC_QUEUE_CHANNEL).
const genericBucket = "#__IRC__#"

// maxWait is the cap on rate-limit delays: no silly long waits.
const maxWait = 3 * time.Second

// longLineUnit: every started 80 bytes of a long line costs an extra
// rate-limit event.
const longLineUnit = 80

var privmsgTargetRe = regexp.MustCompile(`^PRIVMSG (.+?) :`)

// Queue is the per-server outbound queue. Not goroutine-safe: owned by
// the connection writer.
type Queue struct {
	now     func() time.Time
	high    []string
	buckets map[string][]string
	order   []string    // round-robin rotation of buckets with data
	window  []time.Time // rate-limit event timestamps
}

// New returns an empty queue reading time from now.
func New(now func() time.Time) *Queue {
	return &Queue{now: now, buckets: make(map[string][]string)}
}

// Push enqueues a normal-priority line, bucketed by the PRIVMSG target
// channel (anything else shares one generic bucket).
func (q *Queue) Push(line string) {
	bucket := genericBucket
	if m := privmsgTargetRe.FindStringSubmatch(line); m != nil {
		bucket = m[1]
	}
	if _, ok := q.buckets[bucket]; !ok {
		q.order = append(q.order, bucket)
	}
	q.buckets[bucket] = append(q.buckets[bucket], line)
}

// PushHigh enqueues a line ahead of all normal traffic.
func (q *Queue) PushHigh(line string) {
	q.high = append(q.high, line)
}

// Next returns the next line to send. Exactly one of:
//
//	line, 0, true: send line now (its rate-limit cost is recorded)
//	"", wait > 0, false: rate-limited, ask again after wait
//	"", 0, false: queue empty
func (q *Queue) Next() (line string, wait time.Duration, ok bool) {
	if len(q.high) == 0 && len(q.order) == 0 {
		return "", 0, false
	}
	if wait := q.delay(); wait > 0 {
		return "", wait, false
	}
	if len(q.high) > 0 {
		line = q.high[0]
		q.high = q.high[1:]
	} else {
		line = q.popRoundRobin()
	}
	q.window = append(q.window, q.now())
	if len(line) > longLineUnit {
		extra := int(float64(len(line))/longLineUnit + 0.5)
		for range extra {
			q.window = append(q.window, q.now())
		}
	}
	return line, 0, true
}

// popRoundRobin takes the next line from the bucket rotation: a bucket
// with more data goes to the back of the rotation, an emptied bucket
// leaves it (and rejoins on the next Push).
func (q *Queue) popRoundRobin() string {
	bucket := q.order[0]
	q.order = q.order[1:]
	lines := q.buckets[bucket]
	line := lines[0]
	if len(lines) > 1 {
		q.buckets[bucket] = lines[1:]
		q.order = append(q.order, bucket)
	} else {
		delete(q.buckets, bucket)
	}
	return line
}

// delay is the Perl floodGetFloodDelay (1 event per second): positive
// means wait that long (whole seconds, capped); zero means a send is
// allowed now, dropping the oldest spent event.
func (q *Queue) delay() time.Duration {
	if len(q.window) == 0 {
		return 0
	}
	due := q.window[0].Add(time.Duration(len(q.window)) * time.Second)
	wait := due.Sub(q.now()).Truncate(time.Second) // perl int() on whole seconds
	if wait > maxWait {
		return maxWait
	}
	if wait > 0 {
		return wait
	}
	q.window = q.window[1:]
	return 0
}

// Len reports the number of queued lines.
func (q *Queue) Len() int {
	n := len(q.high)
	for _, lines := range q.buckets {
		n += len(lines)
	}
	return n
}
