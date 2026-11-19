package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lease errors.
var (
	ErrLeaseHeld = errors.New("store: lease held by another session")
	ErrLeaseLost = errors.New("store: lease lost")
)

var leaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Lease is a singleton role (such as "executor") held through a
// session-level advisory lock on a dedicated connection. The server drops a
// vanished holder's session, and with it the lock, after about 25 to 30
// seconds (TCP keepalive 10 s + 3 × 5 s, tcp_user_timeout 30 s); until then
// no other process can take the lease. Epoch is the
// fencing token: it is larger for every new holder, and the database
// rejects intent and order writes stamped with an older epoch.
//
// The holder must stop acting as soon as Lost is closed: the session (and
// with it the advisory lock) may already be gone, and another process may
// hold a newer epoch. Fencing makes a late write fail; it does not stop a
// late call to the exchange, so the executor checks Lost before every
// exchange call and persists the intent first (E-2).
type Lease struct {
	name     string
	epoch    int64
	conn     *pgx.Conn
	lost     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	err      error
	closed   bool
	stopOnce sync.Once
}

// LeaseOptions tunes liveness checking; zero values select the defaults.
type LeaseOptions struct {
	PingInterval time.Duration // 0: 2s
	PingTimeout  time.Duration // 0: 3s
}

// AcquireLease tries to take the named lease once. It returns ErrLeaseHeld
// when another session holds it.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, opts LeaseOptions) (*Lease, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	if !leaseName.MatchString(name) {
		return nil, fmt.Errorf("store: invalid lease name %q", name)
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = 2 * time.Second
	}
	if opts.PingTimeout <= 0 {
		opts.PingTimeout = 3 * time.Second
	}
	cc := s.cfg.ConnConfig.Copy()
	// Let the server notice a vanished client quickly, so that a crashed
	// holder's advisory lock is released in seconds rather than after the
	// operating system's default keepalive (two hours on Linux).
	cc.RuntimeParams["tcp_keepalives_idle"] = "10"
	cc.RuntimeParams["tcp_keepalives_interval"] = "5"
	cc.RuntimeParams["tcp_keepalives_count"] = "3"
	// Unacknowledged data (a half-open connection) is also bounded: without
	// this the kernel's retransmission timeout applies (many minutes).
	cc.RuntimeParams["tcp_user_timeout"] = "30000"
	cc.RuntimeParams["application_name"] = "traderd-lease-" + name
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("store: lease connect: %w", err)
	}
	var epoch *int64
	if err := conn.QueryRow(ctx, `SELECT trader_acquire_lease($1, $2)`, name, holder).Scan(&epoch); err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("store: acquire lease: %w", classify(err))
	}
	if epoch == nil {
		conn.Close(context.WithoutCancel(ctx))
		return nil, ErrLeaseHeld
	}
	l := &Lease{name: name, epoch: *epoch, conn: conn, lost: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	go l.monitor(opts)
	return l, nil
}

// Name returns the lease name.
func (l *Lease) Name() string { return l.name }

// Epoch returns the fencing token.
func (l *Lease) Epoch() int64 { return l.epoch }

// Lost is closed when the lease is no longer held (connection failure or
// Release).
func (l *Lease) Lost() <-chan struct{} { return l.lost }

// Err returns why the lease was lost, or nil.
func (l *Lease) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// monitor pings the lease connection; the first failure ends the lease.
func (l *Lease) monitor(opts LeaseOptions) {
	defer close(l.done)
	t := time.NewTicker(opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), opts.PingTimeout)
			err := l.conn.Ping(ctx)
			cancel()
			if err != nil {
				l.fail(fmt.Errorf("%w: %v", ErrLeaseLost, err))
				return
			}
		}
	}
}

func (l *Lease) fail(err error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.err = err
	l.mu.Unlock()
	close(l.lost)
	l.conn.Close(context.Background())
}

// Release unlocks and closes the lease connection. The epoch stays; the
// next holder gets a larger one.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		<-l.done
		return nil
	}
	l.mu.Unlock()
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done // the monitor no longer uses the connection
	l.mu.Lock()
	if l.closed { // the monitor failed it, or a concurrent Release finished
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.err = ErrLeaseLost
	l.mu.Unlock()
	close(l.lost)
	var released bool
	err := l.conn.QueryRow(ctx, `SELECT trader_release_lease($1)`, l.name).Scan(&released)
	l.conn.Close(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("store: release lease: %w", err)
	}
	return nil
}
