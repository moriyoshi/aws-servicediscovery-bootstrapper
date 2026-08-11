package main

import "time"

// spawnConfig holds the respawn/supervision settings for one spawn() call. It
// starts from defaultSpawnConfig() and is overridden by spawn()'s kwargs.
type spawnConfig struct {
	respawnEnabled         bool
	respawnKeepAlive       bool
	respawnMaxRetries      int
	respawnInitialInterval time.Duration
	respawnMaxInterval     time.Duration
	respawnMultiplier      float64
	respawnResetAfter      time.Duration
	shutdownGrace          time.Duration
	preStopTimeout         time.Duration
	resolveTimeout         time.Duration
	resolveFailure         string
	restartOnLiveness      bool
}

// defaultSpawnConfig returns the built-in defaults for a spawn() call. Respawning
// is off unless respawn=True is passed.
func defaultSpawnConfig() *spawnConfig {
	return &spawnConfig{
		respawnEnabled:         false,
		respawnKeepAlive:       false,
		respawnMaxRetries:      5,
		respawnInitialInterval: time.Second,
		respawnMaxInterval:     60 * time.Second,
		respawnMultiplier:      2.0,
		respawnResetAfter:      30 * time.Second,
		shutdownGrace:          10 * time.Second,
		preStopTimeout:         0,
		resolveTimeout:         0,
		resolveFailure:         "retry",
		restartOnLiveness:      true,
	}
}
