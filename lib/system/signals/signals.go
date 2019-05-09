/*
Copyright 2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// package signals implements support for managing interrupt signals
package signals

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/utils"

	log "github.com/sirupsen/logrus"
)

// WatchTerminationSignals stops the provided stopper when it gets one of monitored signals.
// It is a convenience wrapper over NewInterruptHandler
func WatchTerminationSignals(cancel context.CancelFunc, stopper Stopper, printer utils.Printer) *InterruptHandler {
	interrupt := NewInterruptHandler(cancel)
	interrupt.AddStopper(stopper)
	go func() {
		for {
			select {
			case sig := <-interrupt.C:
				printer.Println("Received", sig, "signal, terminating.")
				interrupt.Abort()
			case <-interrupt.Done():
				return
			}
		}
	}()
	return interrupt
}

// NewInterruptHandler creates a new interrupt handler for the specified configuration.
// Returned handler can be used to queue stoppers to the termination loop later by
// using AddStopper:
//
// interrupt := NewInterruptHandler(...)
// interrupt.AddStopper(stoppers...)
//
// Handler will stop all registered stoppers and exit iff:
//  - specified context has expired
//  - handler has been explicitly interrupted (see Trigger)
// If a stopper additionally implements Aborter and the interrupt handler has been explicitly
// interrupted via Trigger, the handler will invoke Abort on the stopper.
//
// Use the select loop and handle the receives on the interrupt channel:
//
// ctx, cancel := ...
// interrupt := NewInterruptHandler(cancel)
// defer interrupt.Close()
// for {
// 	select {
//  	case <-interrupt.C:
// 		if shouldTerminate() [
//			interrupt.Abort()
// 		}
// 	case <-interrupt.Done():
//		// Done
//		return
// 	}
// }
func NewInterruptHandler(cancel context.CancelFunc, opts ...InterruptOption) *InterruptHandler {
	ctx, localCancel := context.WithCancel(context.Background())
	var stoppers []Stopper
	interruptC := make(chan os.Signal)
	termC := make(chan []Stopper, 1)
	handler := &InterruptHandler{
		C:       interruptC,
		ctx:     ctx,
		cancel:  localCancel,
		termC:   termC,
		signals: defaultSignals,
	}
	for _, opt := range opts {
		opt(handler)
	}
	signalC := make(chan os.Signal, 1)
	signal.Notify(signalC, handler.signals...)
	handler.wg.Add(1)
	go func() {
		defer func() {
			signal.Reset(handler.signals...)
			// Reset the signal handler so the next signal is handled
			// directly by the runtime
			if len(stoppers) == 0 {
				handler.wg.Done()
				return
			}
			localCtx, cancel := context.WithTimeout(context.Background(), defaults.ShutdownTimeout)
			for _, stopper := range stoppers {
				if aborter, ok := stopper.(Aborter); ok && handler.isInterrupted() {
					if err := aborter.Abort(localCtx); err != nil {
						log.WithError(err).Warn("Failed to abort stopper.")
					}
				} else {
					if err := stopper.Stop(localCtx); err != nil {
						log.WithError(err).Warn("Failed to stop stopper.")
					}
				}
			}
			cancel()
			handler.wg.Done()
		}()
		for {
			select {
			case handlers := <-termC:
				stoppers = append(stoppers, handlers...)
			case sig := <-signalC:
				select {
				case interruptC <- sig:
				case <-ctx.Done():
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return handler
}

// Close closes the loop and waits until all internal processes have stopped
func (r *InterruptHandler) Close() {
	r.cancel()
	r.wg.Wait()
}

// Done returns the channel that signals when this handler
// is closed
func (r *InterruptHandler) Done() <-chan struct{} {
	return r.ctx.Done()
}

// Abort sets the interrupted flag and interrupts the loop
func (r *InterruptHandler) Abort() {
	r.mu.Lock()
	r.interrupted = true
	r.mu.Unlock()
	r.cancel()
}

// Cancel interrupts the loop without setting the interrupted flag
func (r *InterruptHandler) Cancel() {
	r.cancel()
}

// Add adds stoppers to the internal termination loop
func (r *InterruptHandler) AddStopper(stoppers ...Stopper) {
	select {
	case r.termC <- stoppers:
		// Added new stopper
	case <-r.ctx.Done():
	}
}

// InterruptHandler defines an interruption signal handler
type InterruptHandler struct {
	// C is the channel that receives interrupt requests
	C       <-chan os.Signal
	ctx     context.Context
	cancel  context.CancelFunc
	termC   chan<- []Stopper
	signals []os.Signal
	wg      sync.WaitGroup
	mu      sync.Mutex
	// interrupted is set if the loop has been interrupted
	interrupted bool
}

// interrupted returns true if the handler was interrupted explicitly
func (r *InterruptHandler) isInterrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted
}

// WithSignals specifies which signal to consider interrupt signals.
func WithSignals(signals ...os.Signal) InterruptOption {
	return func(h *InterruptHandler) {
		h.signals = signals
	}
}

// InterruptOption is a functional option to configure interrupt handler
type InterruptOption func(*InterruptHandler)

// Stopper is an interface for processes that can be stopped
type Stopper interface {
	// Stop gracefully stops a process
	Stop(ctx context.Context) error
}

// Aborter is an interface for processes that can be aborted
type Aborter interface {
	// Abort aborts a process
	Abort(ctx context.Context) error
}

// Stop implements Stopper
func (r AborterFunc) Stop(ctx context.Context) error {
	return r(ctx, false)
}

// Abort implements Aborter
func (r AborterFunc) Abort(ctx context.Context) error {
	return r(ctx, true)
}

// AborterFunc is an adapter function that allows the use
// of ordinary functions as both Stoppers and Aborters
type AborterFunc func(ctx context.Context, interrupted bool) error

// Stop implements Stopper
func (r StopperFunc) Stop(ctx context.Context) error {
	return r(ctx)
}

// StopperFunc is an adapter function that allows the use
// of ordinary functions as Stoppers
type StopperFunc func(ctx context.Context) error

// defaultSignals lists default interruption signals
var defaultSignals = []os.Signal{
	syscall.SIGINT,
	syscall.SIGTERM,
	syscall.SIGQUIT,
}
