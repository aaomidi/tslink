package tailscale

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// SupervisorStatus represents the current state of the supervisor.
type SupervisorStatus string

const (
	StatusStopped   SupervisorStatus = "stopped"
	StatusStarting  SupervisorStatus = "starting"
	StatusRunning   SupervisorStatus = "running"
	StatusFailed    SupervisorStatus = "failed"
	StatusCrashLoop SupervisorStatus = "crash_loop"
)

// Backoff configuration.
const (
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2.0

	// Crash loop detection: 5 crashes in 30 seconds.
	crashLoopWindow    = 30 * time.Second
	crashLoopThreshold = 5

	// Health check interval.
	healthCheckInterval = 5 * time.Second

	// Minimum uptime before backoff is reset (prevents rapid restart loops from resetting backoff).
	minUptimeForBackoffReset = 30 * time.Second

	// Startup jitter to prevent thundering herd on host reboot (0-500ms).
	maxStartupJitter = 500 * time.Millisecond
)

// DaemonSupervisor manages a Daemon with automatic restart on failure.
type DaemonSupervisor struct {
	cfg DaemonConfig

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// State (protected by mu)
	mu            sync.RWMutex
	daemon        *Daemon
	status        SupervisorStatus
	restartCount  int
	crashTimes    []time.Time
	lastStartTime time.Time // When daemon last started (for uptime-based backoff reset)

	// Backoff state
	backoffAttempt int

	// Channel to signal initial startup complete (protected by Once)
	startupDone     chan error
	startupDoneOnce sync.Once
}

// NewDaemonSupervisor creates a new supervisor for managing a tailscaled daemon.
func NewDaemonSupervisor(cfg DaemonConfig) *DaemonSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &DaemonSupervisor{
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		status:      StatusStopped,
		crashTimes:  make([]time.Time, 0),
		startupDone: make(chan error, 1),
	}
}

// Start begins the supervision loop and waits for initial daemon startup.
// Returns error if the initial startup fails.
func (s *DaemonSupervisor) Start() error {
	s.mu.Lock()
	if s.status != StatusStopped {
		s.mu.Unlock()
		return fmt.Errorf("supervisor already running (status=%s)", s.status)
	}
	s.status = StatusStarting
	s.mu.Unlock()

	// Start supervision loop in background
	s.wg.Add(1)
	go s.supervisionLoop()

	// Wait for initial startup result
	select {
	case err := <-s.startupDone:
		if err != nil {
			return fmt.Errorf("initial daemon startup failed: %w", err)
		}
		return nil
	case <-time.After(90 * time.Second):
		s.cancel()
		return fmt.Errorf("timeout waiting for initial daemon startup")
	}
}

// Stop gracefully stops the supervisor and daemon.
func (s *DaemonSupervisor) Stop() error {
	logger.Info("Stopping supervisor for endpoint %s", s.cfg.EndpointID[:8])

	// Signal supervision loop to stop
	s.cancel()

	// Wait for supervision loop to finish (with timeout)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debug("Supervisor stopped cleanly")
	case <-time.After(10 * time.Second):
		logger.Warn("Timeout waiting for supervisor to stop")
	}

	s.mu.Lock()
	s.status = StatusStopped
	s.mu.Unlock()

	return nil
}

// Status returns the current supervisor status and restart count.
func (s *DaemonSupervisor) Status() (SupervisorStatus, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.restartCount
}

// GetDaemon returns the current daemon (may be nil if not running).
func (s *DaemonSupervisor) GetDaemon() *Daemon {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.daemon
}

// signalStartup safely sends the startup result exactly once.
func (s *DaemonSupervisor) signalStartup(err error) {
	s.startupDoneOnce.Do(func() {
		s.startupDone <- err
	})
}

// applyStartupJitter adds random delay to prevent thundering herd on mass restart.
func (s *DaemonSupervisor) applyStartupJitter() {
	jitter := time.Duration(rand.Int63n(int64(maxStartupJitter)))
	if jitter > 0 {
		logger.Debug("Applying startup jitter: %v", jitter)
		time.Sleep(jitter)
	}
}

// netnsExists checks if the network namespace still exists.
func (s *DaemonSupervisor) netnsExists() bool {
	_, err := os.Stat(s.cfg.NetNSPath)
	return err == nil
}

// supervisionLoop is the main loop that manages daemon lifecycle.
func (s *DaemonSupervisor) supervisionLoop() {
	defer s.wg.Done()

	firstStart := true

	// Apply jitter on first start to prevent thundering herd
	s.applyStartupJitter()

	for {
		select {
		case <-s.ctx.Done():
			s.stopDaemon()
			return
		default:
		}

		// Check if network namespace still exists (container might be gone)
		if !s.netnsExists() {
			logger.Error("Network namespace %s no longer exists, stopping supervisor", s.cfg.NetNSPath)
			s.mu.Lock()
			s.status = StatusFailed
			s.mu.Unlock()
			s.signalStartup(fmt.Errorf("network namespace no longer exists"))
			return
		}

		// Apply backoff (skip on first attempt)
		if !firstStart {
			// Record crash and check for crash loop AFTER recording
			s.recordCrash()

			if s.isCrashLoop() {
				s.mu.Lock()
				s.status = StatusCrashLoop
				s.mu.Unlock()
				logger.Error("Crash loop detected for endpoint %s (5+ crashes in 30s), stopping recovery",
					s.cfg.EndpointID[:8])
				s.signalStartup(fmt.Errorf("crash loop detected"))
				s.stopDaemon()
				return
			}

			delay := s.nextBackoff()
			logger.Info("Restarting tailscaled in %v (attempt %d)", delay, s.restartCount+1)

			select {
			case <-s.ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		// Start daemon
		s.mu.Lock()
		s.status = StatusStarting
		s.mu.Unlock()

		err := s.startDaemon()
		if err != nil {
			logger.Error("Failed to start tailscaled: %v", err)

			if firstStart {
				s.signalStartup(err)
				firstStart = false
			}
			continue
		}

		// Daemon started successfully - record start time
		startTime := time.Now()
		s.mu.Lock()
		s.status = StatusRunning
		s.restartCount++
		s.lastStartTime = startTime
		s.mu.Unlock()

		if firstStart {
			s.signalStartup(nil)
			firstStart = false
		}

		logger.Info("tailscaled running (restart count: %d)", s.restartCount)

		// Monitor daemon until it exits or we're stopped
		exitErr := s.waitForExit()
		if exitErr == nil {
			// Clean exit (Stop() was called)
			return
		}

		// Unexpected exit - check uptime before resetting backoff
		uptime := time.Since(startTime)
		if uptime >= minUptimeForBackoffReset {
			s.resetBackoff()
			logger.Debug("Backoff reset after %v uptime", uptime)
		}

		logger.Warn("tailscaled exited unexpectedly after %v: %v", uptime, exitErr)
	}
}

// startDaemon creates and starts a new daemon instance.
func (s *DaemonSupervisor) startDaemon() error {
	daemon, err := NewDaemon(s.cfg)
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	if err := daemon.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	s.mu.Lock()
	s.daemon = daemon
	s.mu.Unlock()

	return nil
}

// stopDaemon stops the current daemon if running.
func (s *DaemonSupervisor) stopDaemon() {
	s.mu.Lock()
	daemon := s.daemon
	s.daemon = nil
	s.mu.Unlock()

	if daemon != nil {
		if err := daemon.Stop(); err != nil {
			logger.Warn("Error stopping daemon: %v", err)
		}
	}
}

// waitForExit monitors the daemon and returns when it exits.
// Returns nil if context was cancelled (clean shutdown).
// Returns error if daemon exited unexpectedly.
func (s *DaemonSupervisor) waitForExit() error {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			s.stopDaemon()
			return nil

		case <-ticker.C:
			s.mu.RLock()
			daemon := s.daemon
			s.mu.RUnlock()

			if daemon == nil {
				return fmt.Errorf("daemon is nil")
			}

			if !daemon.IsRunning() {
				return fmt.Errorf("daemon process exited")
			}
		}
	}
}

// isCrashLoop checks if we've had too many crashes in the time window.
func (s *DaemonSupervisor) isCrashLoop() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-crashLoopWindow)
	count := 0
	for _, t := range s.crashTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= crashLoopThreshold
}

// recordCrash records a crash timestamp for crash-loop detection.
func (s *DaemonSupervisor) recordCrash() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.crashTimes = append(s.crashTimes, now)

	// Prune old crash times (keep last 10)
	if len(s.crashTimes) > 10 {
		s.crashTimes = s.crashTimes[len(s.crashTimes)-10:]
	}
}

// nextBackoff returns the next backoff duration using exponential backoff.
func (s *DaemonSupervisor) nextBackoff() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backoffAttempt == 0 {
		s.backoffAttempt = 1
		return initialBackoff
	}

	delay := time.Duration(float64(initialBackoff) * math.Pow(backoffFactor, float64(s.backoffAttempt)))
	s.backoffAttempt++
	return min(delay, maxBackoff)
}

// resetBackoff resets the backoff counter after a successful start.
func (s *DaemonSupervisor) resetBackoff() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoffAttempt = 0
}

// WaitForIP waits for the daemon to get a Tailscale IP.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) WaitForIP() (*Status, error) {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return nil, fmt.Errorf("no daemon running")
	}

	return daemon.WaitForIP()
}

// SetHostname updates the Tailscale hostname.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) SetHostname(hostname string) error {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return fmt.Errorf("no daemon running")
	}

	return daemon.SetHostname(hostname)
}

// ConfigureServeEndpoints configures serve endpoints.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) ConfigureServeEndpoints(
	service string,
	endpoints []ServeEndpoint,
	tags []string,
	direct bool,
) error {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return fmt.Errorf("no daemon running")
	}

	return daemon.ConfigureServeEndpoints(service, endpoints, tags, direct)
}
