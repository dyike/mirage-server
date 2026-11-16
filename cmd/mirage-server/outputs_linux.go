package main

import (
	"context"
	"github.com/dyike/mirage-server/internal/server/input"
	"log"
	"slices"
	"time"
)

type outputDiscovery func() ([]input.OutputInfo, error)

const outputPollInterval = 500 * time.Millisecond

// Two equal snapshots debounce compositor mode changes and output hotplug.
type outputChange struct {
	pending []input.OutputInfo
	count   int
}

func (c *outputChange) observe(current, next []input.OutputInfo, err error) bool {
	if err != nil || slices.Equal(current, next) {
		c.pending = nil
		c.count = 0
		return false
	}
	if c.count == 0 || !slices.Equal(c.pending, next) {
		c.pending = slices.Clone(next)
		c.count = 1
		return false
	}
	c.count++
	return c.count >= 2
}
func watchOutputChanges(ctx context.Context, current []input.OutputInfo, discover outputDiscovery) bool {
	ticker := time.NewTicker(outputPollInterval)
	defer ticker.Stop()
	var change outputChange
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			next, err := discover()
			if change.observe(current, next, err) {
				return ctx.Err() == nil
			}
		}
	}
}
func waitForOutputs(ctx context.Context, discover outputDiscovery) ([]input.OutputInfo, error) {
	var previous []input.OutputInfo
	announced := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := discover()
		if err == nil && len(next) > 0 && slices.Equal(previous, next) {
			return next, nil
		}
		if err == nil {
			previous = slices.Clone(next)
		} else {
			previous = nil
		}
		if !announced && (err != nil || len(next) == 0) {
			log.Print("Waiting for an active Wayland output")
			announced = true
		}
		timer := time.NewTimer(outputPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
