package main

import (
	"context"
	"fmt"
	"time"

	"github.com/franklad/runner"
)

type Ping struct {
	done chan struct{}
}

func NewPing() *Ping {
	return &Ping{
		done: make(chan struct{}),
	}
}

func (p *Ping) Run(ctx context.Context) error {
	// Simulate a long-running process
	for {
		select {
		case <-p.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.Tick(1 * time.Second):
			fmt.Println("Ping")
		}
	}
}

func (p *Ping) Stop(ctx context.Context) error {
	// Simulate stopping the process
	close(p.done)
	return nil
}

func (p *Ping) Name() string {
	return "Ping"
}

func main() {
	ctx := context.Background()
	processes := []runner.Process{
		NewPing(),
	}

	runner.New(ctx, processes...).Run(ctx).ThenStop()
}
