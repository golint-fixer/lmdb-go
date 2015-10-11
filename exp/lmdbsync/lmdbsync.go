/*
Package lmdbsync provides advanced synchronization for LMDB environments at the
cost of performance.  The package provides a drop-in replacement for *lmdb.Env
that can be used in situations where the database may be resized or where the
flag lmdb.NoLock is used.

Bypassing an Env's methods to access the underlying lmdb.Env is safe.  The
severity of such usage depends such behavior should be strictly avoided as it
may produce undefined behavior from the LMDB C library.

Resizing the environment

The Env type intercepts any MapResized error returned from a transaction and
transparently handles it, retrying the transaction after the new size has been
adopted.  All synchronization is handled so running transactions complete
before SetMapSize is called on the underlying lmdb.Env.

However, ppplications are recommended against attempting to change the memory
map size for an open database.  It requires careful synchronization by all
processes accessing the database file.  And, a large memory map will not affect
disk usage on operating systems that support sparse files (e.g. Linux, not OS
X).

NoLock

The lmdb.NoLock flag performs all transaction synchronization with Go
structures and is an experimental feature.  It is unclear what benefits this
provides.
*/
package lmdbsync

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bmatsuo/lmdb-go/lmdb"
)

// The default number of times to retry a transaction that is returning
// repeatedly MapResized. This signifies rapid database growth from another
// process or some bug/corruption in memory.
//
// If DefaultRetryResize is less than zero the transaction will be retried
// indefinitely.
var DefaultRetryResize = 2

// If a transaction returns MapResize DefaultRetryResize times consequtively an
// Env will stop attempting to run it and return MapResize to the caller.
var DefaultDelayRepeatResize = time.Millisecond

// Env wraps an *lmdb.Env, excepting the same methods, but provides transaction
// management for advanced usage of LMDB.  Transactions run by Env handle
// lmdb.MapResized error transparently through additional synchronization.
// Additionally, Env is safe to use on environments setting the lmdb.NoLock
// flag.  When in NoLock mode write transactions block all read transactions
// from running (in addition to blocking other write transactions like a normal
// lmdb.Env would).
//
// Env proxies several methods to provide synchronization required for safe
// operation in advanced usage scenarios.  It is important not to call proxied
// methods directly on the underlying lmdb.Env or synchronization may be
// interfered with.  Calling proxied methods directly on the lmdb.Env may
// result in poor transaction performance or undefined behavior in from the C
// library.
type Env struct {
	*lmdb.Env
	// RetryResize overrides DefaultRetryResize for the Env.
	RetryResize int
	// DelayRepeateResize overrides DefaultDelayRetryResize for the Env.
	DelayRepeatResize func(retry int) time.Duration
	noLock            bool
	txnlock           sync.RWMutex
}

// NewEnv returns an newly allocated Env that wraps env.  If env is nil then
// lmdb.NewEnv() will be called to allocate an lmdb.Env.
func NewEnv(env *lmdb.Env) (*Env, error) {
	var err error
	if env == nil {
		env, err = lmdb.NewEnv()
		if err != nil {
			return nil, err
		}
	}

	flags, err := env.Flags()
	if lmdb.IsErrnoSys(err, syscall.EINVAL) {
		err = nil
	} else if err != nil {
		return nil, err
	}
	noLock := flags&lmdb.NoLock != 0

	_env := &Env{
		Env:    env,
		noLock: noLock,
	}
	return _env, nil
}

// Open is a proxy for r.Env.Open() that detects the lmdb.NoLock flag to
// properly manage transaction synchronization.
func (r *Env) Open(path string, flags uint, mode os.FileMode) error {
	err := r.Env.Open(path, flags, mode)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = true
	}

	return nil
}

// SetFlags is a proxy for r.Env.SetFlags() that detects the lmdb.NoLock flag
// to properly manage transaction synchronization.
func (r *Env) SetFlags(flags uint) error {
	err := r.Env.SetFlags(flags)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = true
	}

	return nil
}

// UnsetFlags is a proxy for r.Env.UnsetFlags() that detects the lmdb.NoLock flag
// to properly manage transaction synchronization.
func (r *Env) UnsetFlags(flags uint) error {
	err := r.Env.UnsetFlags(flags)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = false
	}

	return nil
}

// SetMapSize is a proxy for r.Env.SetMapSize() that blocks while concurrent
// transactions are in progress.
func (r *Env) SetMapSize(size int64) error {
	r.txnlock.Lock()
	err := r.setMapSize(size, 0)
	r.txnlock.Unlock()
	return err
}

func (r *Env) setMapSize(size int64, delay time.Duration) error {
	r.txnlock.Lock()
	if delay > 0 {
		// wait before adopting a map size set from another process. hold on to
		// the transaction lock so that other transactions don't attempt to
		// begin while waiting.
		time.Sleep(delay)
	}
	err := r.Env.SetMapSize(0)
	r.txnlock.Unlock()
	return err
}

// RunTxn is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then RunTxn will block while other updates
// are in progress, regardless of flags.
//
// If RunTxn returns MapResized it means another process(es) was writing too
// fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) RunTxn(flags uint, op lmdb.TxnOp) (err error) {
	readonly := flags&lmdb.Readonly != 0
	return r.runRetry(readonly, func() error { return r.Env.RunTxn(flags, op) })
}

// View is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then View will block until any running update
// completes.
//
// If View returns MapResized it means another process(es) was writing too fast
// to the database and the calling process could not get a valid transaction
// handle.
func (r *Env) View(op lmdb.TxnOp) error {
	return r.runRetry(true, func() error { return r.Env.View(op) })
}

// Update is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then Update blocks until all other
// transactions have terminated and blocks all other transactions from running
// while in progress (including readonly transactions).
//
// If Update returns MapResized it means another process(es) was writing too
// fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) Update(op lmdb.TxnOp) error {
	return r.runRetry(false, func() error { return r.Env.Update(op) })
}

// UpdateLocked is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then UpdateLocked blocks until all other
// transactions have terminated and blocks all other transactions from running
// while in progress (including readonly transactions).
//
// If UpdateLocked returns MapResized it means another process(es) was writing
// too fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) UpdateLocked(op lmdb.TxnOp) error {
	return r.runRetry(false, func() error { return r.Env.UpdateLocked(op) })
}

func (r *Env) runRetry(readonly bool, fn func() error) error {
	var err error
	for i := 0; ; i++ {
		err = r.run(readonly, fn)
		if !r.retryResized(i, err) {
			return err
		}
	}
	return err
}

func (r *Env) run(readonly bool, fn func() error) error {
	var err error
	if r.noLock && !readonly {
		r.txnlock.Lock()
		err = fn()
		r.txnlock.Unlock()
	} else {
		r.txnlock.RLock()
		err = fn()
		r.txnlock.RUnlock()
	}
	return err
}

func (r *Env) getRetryResize() int {
	if r.RetryResize != 0 {
		return r.RetryResize
	}
	return DefaultRetryResize
}

func (r *Env) getDelayRepeatResize(i int) time.Duration {
	if r.DelayRepeatResize != nil {
		return r.DelayRepeatResize(i)
	}
	return DefaultDelayRepeatResize
}

func (r *Env) retryResized(i int, err error) bool {
	if !lmdb.IsMapResized(err) {
		return false
	}

	// fail the transaction with MapResized error when too many attempts have
	// been made.
	maxRetry := r.getRetryResize()
	if maxRetry <= 0 {
		return false
	}
	if maxRetry < i {
		return false
	}

	var delay time.Duration
	if i > 0 {
		delay = r.getDelayRepeatResize()
	}

	err = r.setMapSize(0, delay)
	if err != nil {
		panic(err)
	}
	return true
}
