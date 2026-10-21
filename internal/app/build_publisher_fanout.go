package app

import "github.com/yourname/dayz-killfeed/internal/killfeed"

// buildPublisherFanout hands each build action to several consumers (the
// BUILD_FEED channel and the Base Raid Alarm). Each consumer is non-blocking.
type buildPublisherFanout []killfeed.BuildPublisher

func (f buildPublisherFanout) PublishBuild(ev *killfeed.Event) {
	for _, p := range f {
		if p != nil {
			p.PublishBuild(ev)
		}
	}
}
