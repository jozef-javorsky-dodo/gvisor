// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ptrace provides a ptrace-based implementation of the platform
// interface. This is useful for development and testing purposes primarily,
// and runs on stock kernels without special permissions.
//
// In a nutshell, it works as follows:
//
// The creation of a new address space creates a new child process with a single
// thread which is traced by a single goroutine.
//
// A context is just a collection of temporary variables. Calling Switch on a
// context does the following:
//
//	Locks the runtime thread.
//
//	Looks up a traced subprocess thread for the current runtime thread. If
//	none exists, the dedicated goroutine is asked to create a new stopped
//	thread in the subprocess. This stopped subprocess thread is then traced
//	by the current thread and this information is stored for subsequent
//	switches.
//
//	The context is then bound with information about the subprocess thread
//	so that the context may be appropriately interrupted via a signal.
//
//	The requested operation is performed in the traced subprocess thread
//	(e.g. set registers, execute, return).
//
// Lock order:
//
//	 subprocess.mu
//		context.mu
package ptrace

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/interrupt"
	"gvisor.dev/gvisor/pkg/sync"
)

var (
	// stubStart is the link address for our stub, and determines the
	// maximum user address. This is valid only after a call to stubInit.
	//
	// We attempt to link the stub here, and adjust downward as needed.
	stubStart uintptr = stubInitAddress

	// stubEnd is the first byte past the end of the stub, as with
	// stubStart this is valid only after a call to stubInit.
	stubEnd uintptr

	// stubInitialized controls one-time stub initialization.
	stubInitialized sync.Once
)

type context struct {
	archContext

	// signalInfo is the signal info, if and when a signal is received.
	signalInfo linux.SignalInfo

	// interrupt is the interrupt context.
	interrupt interrupt.Forwarder

	// mu protects the following fields.
	mu sync.Mutex

	// If lastFaultSP is non-nil, the last context switch was due to a fault
	// received while executing lastFaultSP. Only context.Switch may set
	// lastFaultSP to a non-nil value.
	lastFaultSP *subprocess

	// lastFaultAddr is the last faulting address; this is only meaningful if
	// lastFaultSP is non-nil.
	lastFaultAddr hostarch.Addr

	// lastFaultIP is the address of the last faulting instruction;
	// this is also only meaningful if lastFaultSP is non-nil.
	lastFaultIP hostarch.Addr
}

// NewContext implements platform.Platform.NewContext.
func (*PTrace) NewContext(ctx pkgcontext.Context) platform.Context {
	c := new(context)
	c.archContext.init(ctx)
	return c
}

// Switch runs the provided context in the given address space.
func (c *context) Switch(ctx pkgcontext.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	as := mm.AddressSpace()
	s := as.(*subprocess)
restart:
	isSyscall := s.switchToApp(c, ac)

	var (
		faultSP   *subprocess
		faultAddr hostarch.Addr
		faultIP   hostarch.Addr
	)
	if !isSyscall && linux.Signal(c.signalInfo.Signo) == linux.SIGSEGV {
		faultSP = s
		faultAddr = hostarch.Addr(c.signalInfo.Addr())
		faultIP = hostarch.Addr(ac.IP())
	}

	// Update the context to reflect the outcome of this context switch.
	c.mu.Lock()
	lastFaultSP := c.lastFaultSP
	lastFaultAddr := c.lastFaultAddr
	lastFaultIP := c.lastFaultIP
	// At this point, c may not yet be in s.contexts, so c.lastFaultSP won't be
	// updated by s.Unmap(). This is fine; we only need to synchronize with
	// calls to s.Unmap() that occur after the handling of this fault.
	c.lastFaultSP = faultSP
	c.lastFaultAddr = faultAddr
	c.lastFaultIP = faultIP
	c.mu.Unlock()

	// Update subprocesses to reflect the outcome of this context switch.
	if lastFaultSP != faultSP {
		if lastFaultSP != nil {
			lastFaultSP.mu.Lock()
			delete(lastFaultSP.contexts, c)
			lastFaultSP.mu.Unlock()
		}
		if faultSP != nil {
			faultSP.mu.Lock()
			faultSP.contexts[c] = struct{}{}
			faultSP.mu.Unlock()
		}
	}

	if isSyscall {
		return nil, hostarch.NoAccess, nil
	}

	si := c.signalInfo
	if faultSP == nil {
		// Non-fault signal.
		return &si, hostarch.NoAccess, platform.ErrContextSignal
	}

	// See if this can be handled as a CPUID instruction.
	if linux.Signal(si.Signo) == linux.SIGSEGV && platform.TryCPUIDEmulate(ctx, mm, ac) {
		goto restart
	}

	// Got a page fault. Ideally, we'd get real fault type here, but ptrace
	// doesn't expose this information. Instead, we use a simple heuristic:
	//
	// It was an instruction fault iff the faulting addr == instruction
	// pointer.
	//
	// It was a write fault if the fault is immediately repeated.
	at := hostarch.Read
	if faultAddr == faultIP {
		at.Execute = true
	}
	if lastFaultSP == faultSP &&
		lastFaultAddr == faultAddr &&
		lastFaultIP == faultIP {
		at.Write = true
	}

	// Handle as a signal.
	return &si, at, platform.ErrContextSignal
}

// Interrupt interrupts the running guest application associated with this context.
func (c *context) Interrupt() {
	c.interrupt.NotifyInterrupt()
}

// Release implements platform.Context.Release().
func (c *context) Release() {}

// FullStateChanged implements platform.Context.FullStateChanged.
func (c *context) FullStateChanged() {}

// PullFullState implements platform.Context.PullFullState.
func (c *context) PullFullState(as platform.AddressSpace, ac *arch.Context64) error { return nil }

// PrepareSleep implements platform.Context.platform.PrepareSleep.
func (*context) PrepareSleep() {}

// PTrace represents a collection of ptrace subprocesses.
type PTrace struct {
	platform.MMapMinAddr
	platform.NoCPUPreemptionDetection
	platform.UseHostGlobalMemoryBarrier
}

// New returns a new ptrace-based implementation of the platform interface.
func New() (*PTrace, error) {
	stubInitialized.Do(func() {
		// Initialize the stub.
		stubInit()

		// Create the master process for the global pool. This must be
		// done before initializing any other processes.
		master, err := newSubprocess(createStub)
		if err != nil {
			// Should never happen.
			panic("unable to initialize ptrace master: " + err.Error())
		}

		// Set the master on the globalPool.
		globalPool.master = master
	})

	return &PTrace{}, nil
}

// SupportsAddressSpaceIO implements platform.Platform.SupportsAddressSpaceIO.
func (*PTrace) SupportsAddressSpaceIO() bool {
	return false
}

// CooperativelySchedulesAddressSpace implements platform.Platform.CooperativelySchedulesAddressSpace.
func (*PTrace) CooperativelySchedulesAddressSpace() bool {
	return false
}

// MapUnit implements platform.Platform.MapUnit.
func (*PTrace) MapUnit() uint64 {
	// The host kernel manages page tables and arbitrary-sized mappings
	// have effectively the same cost.
	return 0
}

// MaxUserAddress returns the first address that may not be used by user
// applications.
func (*PTrace) MaxUserAddress() hostarch.Addr {
	return hostarch.Addr(stubStart)
}

// NewAddressSpace returns a new subprocess.
func (p *PTrace) NewAddressSpace(any) (platform.AddressSpace, <-chan struct{}, error) {
	as, err := newSubprocess(globalPool.master.createStub)
	return as, nil, err
}

type constructor struct{}

func (*constructor) New(platform.Options) (platform.Platform, error) {
	return New()
}

func (*constructor) OpenDevice(_ string) (*fd.FD, error) {
	return nil, nil
}

// Flags implements platform.Constructor.Flags().
func (*constructor) Requirements() platform.Requirements {
	return platform.Requirements{
		RequiresCapSysPtrace: true,
	}
}

func init() {
	platform.Register("ptrace", &constructor{})
}
