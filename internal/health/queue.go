package health

import "time"

type QueueHealth struct {
	Name                           string
	Depth, Capacity, HighWaterMark int
	OldestAge                      time.Duration
	Dropped                        uint64
	State                          State
}

func EvaluateQueue(q QueueHealth) QueueHealth {
	if q.Capacity <= 0 {
		q.State = Unknown
		return q
	}
	if q.Dropped > 0 && q.Name == "persistence" {
		q.State = Unhealthy
	} else if q.Depth*100 >= q.Capacity*80 {
		q.State = Degraded
	} else {
		q.State = Healthy
	}
	return q
}
