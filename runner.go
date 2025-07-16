// Package runner provides a robust process management system for running and gracefully
// shutting down multiple processes in a production environment. It handles signals for
// termination, propagates errors, and ensures all processes are stopped properly.
//
// The Runner manages a set of Processes, starting them concurrently, monitoring for errors
// or context cancellation, and stopping them on receipt of termination signals or errors.
//
// Key Features:
//   - Concurrent execution of processes.
//   - Graceful shutdown with configurable timeout.
//   - Sequential stopping of processes to allow for potential ordering and to manage
//     shutdown in a controlled manner (e.g., for dependencies).
//   - Panic recovery in process execution and monitoring.
//   - Detailed logging of process lifecycle and errors.
//   - Error collection for processes that fail unexpectedly.
//   - Buffered signal channel to prevent dropped signals.
//   - Handling for zero processes to avoid hangs.
//   - Emphasis on Process implementations respecting contexts to prevent shutdown hangs.
//
// Important Notes for Process Implementations:
//   - Run(ctx) should block until stopped or an error occurs, and must respect ctx for
//     internal cancellations (e.g., use select on ctx.Done() for long-running loops).
//   - Stop(ctx) must respect the provided ctx and return promptly on ctx.Done() (e.g.,
//     by propagating ctx to internal shutdown mechanisms and avoiding indefinite blocks).
//     Failure to do so may cause shutdown hangs, leading to forceful termination in
//     production environments (e.g., SIGKILL from orchestrators like Kubernetes).
//   - If Processes have dependencies, provide them in the desired stop order (reverse
//     dependency order) when creating the Runner, as stopping is sequential in the order
//     provided.
//   - Use the provided ctx in Run for spawning internal goroutines, and join them in Stop
//     to prevent orphans or leaks.
//
// Usage:
//
//	ctx := context.Background()
//	runner := NewRunner(ctx, config, processes...)
//	runner.Run(ctx).ThenStop()

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/franklad/logger"
)

// Process defines the interface for managed processes.
// Run should block until the process is stopped or an error occurs.
// Stop should gracefully shutdown the process.
type Process interface {
	// Run starts the process and blocks until stopped or error.
	// It should respect the provided context for any internal cancellations,
	// but primary stopping is via Stop().
	Run(ctx context.Context) error
	// Stop gracefully stops the process.
	Stop(ctx context.Context) error
	// Name returns a unique name for the process.
	Name() string
}

// processError holds error information from a failed process.
type processError struct {
	process Process
	err     error
}

// Runner manages the lifecycle of multiple Processes.
type Runner struct {
	log       logger.Logger
	stop      chan os.Signal // Buffered to prevent dropped signals
	errors    chan processError
	processes []Process
	runWG     sync.WaitGroup // WaitGroup for running process goroutines
	monitorWG sync.WaitGroup // WaitGroup for monitor goroutine
	sigCtx    context.Context
	sigCancel context.CancelFunc
}

// New creates a new Runner instance with the given processes.
// Provide processes in the desired stop order (e.g., reverse dependency order).
func New(ctx context.Context, processes ...Process) *Runner {
	log := logger.New().FromContext(ctx).WithFields("engine", "runner")
	log.Info("initializing runner engine")

	signals := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	sigCtx, sigCancel := signal.NotifyContext(context.Background(), signals...)

	r := &Runner{
		log:       log,
		stop:      make(chan os.Signal, 2), // Buffered to queue signals reliably
		errors:    make(chan processError, len(processes)),
		processes: processes,
		sigCtx:    sigCtx,
		sigCancel: sigCancel,
	}

	signal.Notify(r.stop, signals...)
	return r
}

// Run starts all processes concurrently and begins monitoring.
// It returns the Runner for chaining.
func (r *Runner) Run(ctx context.Context) *Runner {
	if len(r.processes) == 0 {
		r.log.Warn("no processes to run; triggering immediate shutdown")
		go func() {
			r.stop <- syscall.SIGTERM
		}()

		return r
	}

	r.monitorWG.Add(1)
	go func() {
		defer r.monitorWG.Done()
		r.monitor(ctx)
	}()

	for _, p := range r.processes {
		r.runWG.Add(1)
		go r.runProcess(ctx, p)
	}

	return r
}

// ThenStop blocks until a stop signal is received (or internal trigger), then gracefully stops all processes in sequence.
// It collects any errors during stopping.
// After ThenStop returns, the Runner should not be reused.
// Stopping is sequential in the order processes were provided, allowing for dependency management.
func (r *Runner) ThenStop() {
	select {
	case <-r.sigCtx.Done():
		r.log.Info("received external stop signal, initiating graceful shutdown")
	case sig := <-r.stop:
		r.log.Info("received internal stop signal, initiating graceful shutdown", "signal", sig)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, p := range r.processes {
		r.stopProcess(stopCtx, p)
	}

	r.runWG.Wait()
	r.monitorWG.Wait()

	r.sigCancel()
	signal.Stop(r.stop)

	close(r.stop)
	close(r.errors)

	r.log.Info("shutdown complete")
}

// Errors returns a read-only channel for receiving process errors.
func (r *Runner) Errors() <-chan processError {
	return r.errors
}

// runProcess runs a single process with panic recovery.
func (r *Runner) runProcess(ctx context.Context, process Process) {
	defer r.runWG.Done()

	r.log.Info("starting process", "process_name", process.Name())

	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("panic recovered: %v", rec)
			r.log.Error(err, "process panicked", "process_name", process.Name())
			select {
			case r.errors <- processError{process: process, err: err}:
			default:
			}
		}
	}()

	if err := process.Run(ctx); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.log.Error(err, "process failed", "process_name", process.Name())
			select {
			case r.errors <- processError{process: process, err: err}:
			default:
			}
		}

		return
	}

	r.log.Info("process completed normally", "process_name", process.Name())
}

// stopProcess stops a single process with panic recovery.
// Called sequentially in ThenStop.
func (r *Runner) stopProcess(ctx context.Context, process Process) {
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("panic recovered during stop: %v", rec)
			r.log.Error(err, "failed to stop process", "process_name", process.Name())
		}
	}()

	if err := process.Stop(ctx); err != nil {
		r.log.Error(err, "failed to stop process", "process_name", process.Name())
		return
	}

	r.log.Info("stopped process", "process_name", process.Name())
}

// monitor monitors for errors or context cancellation to trigger shutdown.
func (r *Runner) monitor(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("panic recovered in monitor: %v", rec)
			r.log.Error(err, "monitor panicked")
			r.stop <- syscall.SIGTERM
		}
	}()

	select {
	case perr := <-r.errors:
		if perr.err != nil {
			// Error already logged in runProcess
			r.log.Warn("process error detected, sending stop signal")
			r.stop <- syscall.SIGTERM
		}
	case <-ctx.Done():
		r.log.Warn("context canceled, sending stop signal")
		r.stop <- syscall.SIGTERM
	case <-r.sigCtx.Done():
	}
}
