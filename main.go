package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	servicediscovery_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/cenkalti/backoff/v5"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"
)

var namespace string
var healthStatus string
var precondition string
var preconditionCheckTimeout time.Duration
var retryCount int
var noFail bool
var executionDelayJitter time.Duration
var executionDelayJitterUnit time.Duration

// respawn supervisor
var respawnEnabled bool
var respawnKeepAlive bool
var respawnMaxRetries int
var respawnInitialInterval time.Duration
var respawnMaxInterval time.Duration
var respawnMultiplier float64
var respawnResetAfter time.Duration
var shutdownGrace time.Duration

// workload healthcheck
var healthcheckType string
var healthcheckTarget string
var healthcheckInterval time.Duration
var healthcheckTimeout time.Duration
var healthcheckHealthyThreshold int
var healthcheckUnhealthyThreshold int
var healthcheckStartPeriod time.Duration
var healthcheckAction string
var healthcheckGRPCService string

// control socket
var controlSocket string
var healthProbe bool

func init() {
	flag.StringVar(&namespace, "namespace", "", "The namespace of the instance to be listed")
	flag.StringVar(&healthStatus, "health-status", "HEALTHY", "The health status of the instance to be listed")
	flag.StringVar(&precondition, "precondition", "AllEcsTasksRunning", "Precondition that needs to be met before running the command. Supported values: AllEcsTasksRunning")
	flag.DurationVar(&preconditionCheckTimeout, "precondition-check-timeout", 30*time.Second, "The timeout for the precondition check")
	flag.IntVar(&retryCount, "retry-count", 10, "The number of times to retry the request")
	flag.BoolVar(&noFail, "no-fail", false, "Do not fail if no instances are found")
	flag.DurationVar(&executionDelayJitter, "execution-delay-jitter", 0, "The amount of jitter that delays the command execution.")
	flag.DurationVar(&executionDelayJitterUnit, "execution-delay-jitter-unit", time.Second, "The unit of the jitter that delays the command execution")

	flag.BoolVar(&respawnEnabled, "respawn", false, "Restart the workload when it exits with a non-zero status")
	flag.BoolVar(&respawnKeepAlive, "respawn-keep-alive", false, "Also restart the workload when it exits cleanly (implies -respawn semantics for exit code 0)")
	flag.IntVar(&respawnMaxRetries, "respawn-max-retries", 5, "Maximum number of consecutive restarts before giving up (0 = unlimited)")
	flag.DurationVar(&respawnInitialInterval, "respawn-initial-interval", time.Second, "Initial backoff interval between restarts")
	flag.DurationVar(&respawnMaxInterval, "respawn-max-interval", 60*time.Second, "Maximum backoff interval between restarts")
	flag.Float64Var(&respawnMultiplier, "respawn-multiplier", 2.0, "Backoff multiplier between restarts")
	flag.DurationVar(&respawnResetAfter, "respawn-reset-after", 30*time.Second, "If the workload stays up at least this long, the retry/backoff counter is reset")
	flag.DurationVar(&shutdownGrace, "shutdown-grace", 10*time.Second, "Grace period between SIGTERM and SIGKILL when terminating the workload")

	flag.StringVar(&healthcheckType, "healthcheck-type", "", "Workload healthcheck type: http, https, tcp, grpc (empty = disabled)")
	flag.StringVar(&healthcheckTarget, "healthcheck-target", "", "Healthcheck target: URL for http(s), host:port for tcp/grpc")
	flag.DurationVar(&healthcheckInterval, "healthcheck-interval", 10*time.Second, "Interval between healthcheck probes")
	flag.DurationVar(&healthcheckTimeout, "healthcheck-timeout", 2*time.Second, "Timeout for each healthcheck probe (must be less than the interval)")
	flag.IntVar(&healthcheckHealthyThreshold, "healthcheck-healthy-threshold", 1, "Consecutive successful probes required to become healthy")
	flag.IntVar(&healthcheckUnhealthyThreshold, "healthcheck-unhealthy-threshold", 3, "Consecutive failed probes required to become unhealthy")
	flag.DurationVar(&healthcheckStartPeriod, "healthcheck-start-period", 0, "Grace period before the first probe during which failures are ignored")
	flag.StringVar(&healthcheckAction, "healthcheck-action", "none", "Action on sustained unhealthy: none (observational) or restart")
	flag.StringVar(&healthcheckGRPCService, "healthcheck-grpc-service", "", "Service name for the gRPC health check (default: overall server health)")

	flag.StringVar(&controlSocket, "control-socket", "", "Path to a unix-domain control socket serving GET /health (empty = disabled)")
	flag.BoolVar(&healthProbe, "health-probe", false, "Run as a health-probe client against -control-socket and exit 0 (healthy) or non-zero; for use as a container HEALTHCHECK CMD")
}

type entry struct {
	IPv4Addr string
	IPv6Addr string
	Port     int
}

type serviceDiscovery struct {
	svc       *servicediscovery.Client
	namespace string
	hsf       servicediscovery_types.HealthStatusFilter
	maxTries  int
}

func (sd *serviceDiscovery) do(ctx context.Context, service string) ([]entry, error) {
	return backoff.Retry(
		ctx,
		func() ([]entry, error) {
			out, err := sd.svc.DiscoverInstances(ctx, &servicediscovery.DiscoverInstancesInput{
				NamespaceName: aws.String(sd.namespace),
				ServiceName:   aws.String(service),
				HealthStatus:  sd.hsf,
			})
			if err != nil {
				return nil, backoff.Permanent(fmt.Errorf("failed to discover instances: %w", err))
			}
			entries := make([]entry, 0, len(out.Instances))
			for _, instance := range out.Instances {
				ipv4Addr := ""
				ipv6Addr := ""
				port := 0
				if v, ok := instance.Attributes["AWS_INSTANCE_IPV4"]; ok {
					ipv4Addr = v
				}
				if v, ok := instance.Attributes["AWS_INSTANCE_IPV6"]; ok {
					ipv6Addr = v
				}
				if v, ok := instance.Attributes["AWS_INSTANCE_PORT"]; ok {
					port, err = strconv.Atoi(v)
					if err != nil {
						return nil, fmt.Errorf("failed to convert port to int: %w", err)
					}
				}
				entries = append(entries, entry{IPv4Addr: ipv4Addr, IPv6Addr: ipv6Addr, Port: port})
			}
			if len(entries) == 0 {
				return nil, errors.New("no instances found")
			}
			return entries, nil
		},
		backoff.WithBackOff(
			&backoff.ExponentialBackOff{
				InitialInterval:     2 * time.Second,
				RandomizationFactor: backoff.DefaultRandomizationFactor,
				Multiplier:          backoff.DefaultMultiplier,
				MaxInterval:         60 * time.Second,
			},
		),
		backoff.WithMaxTries(uint(sd.maxTries)),
	)
}

type taskMetadataV4 struct {
	Cluster     string `json:"Cluster"`
	ServiceName string `json:"ServiceName"`
	VPCID       string `json:"VPCID"`
	TaskARN     string `json:"TaskARN"`
	Family      string `json:"Family"`
	Revision    string `json:"Revision"`
}

func fetchContainerMetadata(ctx context.Context) (*taskMetadataV4, error) {
	uri := os.Getenv("AWS_CONTAINER_METADATA_URI_V4")
	if uri == "" {
		return nil, fmt.Errorf("AWS_CONTAINER_METADATA_URI_V4 environment variable is not set")
	}
	req, err := http.NewRequest(http.MethodGet, uri+"/task", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build a request: %w", err)
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch container metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch container metadata: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	metadata := new(taskMetadataV4)
	err = json.Unmarshal(b, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal container metadata: %w", err)
	}
	return metadata, nil
}

func waitForECSServiceUp(ctx context.Context, cfg *aws.Config, cluster string, service string, pollInterval time.Duration, timeout time.Duration) error {
	client := ecs.NewFromConfig(*cfg)
	timeoutAt := time.Now().Add(timeout)
	for time.Now().Before(timeoutAt) {
		out, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  &cluster,
			Services: []string{service},
		})
		if err != nil {
			return fmt.Errorf("failed to describe ECS service: %w", err)
		}
		if len(out.Services) == 0 {
			return fmt.Errorf("no ECS service found for %s", service)
		}
		if out.Services[0].RunningCount == out.Services[0].DesiredCount {
			return nil // Service is up and running
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("ECS service %s is not up after %s", service, timeout)
}

func preconditionCheckECSService(ctx context.Context, cfg *aws.Config, pollInterval time.Duration, timeout time.Duration) error {
	metadata, err := fetchContainerMetadata(ctx)
	if err != nil {
		return err
	}
	return waitForECSServiceUp(ctx, cfg, metadata.Cluster, metadata.ServiceName, pollInterval, timeout)
}

var preconditions = map[string]func(context.Context, *aws.Config, time.Duration, time.Duration) error{
	"allecstasksrunning": func(ctx context.Context, cfg *aws.Config, pollInterval time.Duration, timeout time.Duration) error {
		return preconditionCheckECSService(ctx, cfg, pollInterval, timeout)
	},
}

// disableEndpointPrefix applies the flag that will prevent any
// operation-specific host prefix from being applied
type disableEndpointPrefix struct{}

func (disableEndpointPrefix) ID() string { return "disableEndpointPrefix" }

func (disableEndpointPrefix) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	ctx = smithyhttp.SetHostnameImmutable(ctx, true)
	return next.HandleInitialize(ctx, in)
}

func addDisableEndpointPrefix(stack *middleware.Stack) error {
	return stack.Initialize.Add(disableEndpointPrefix{}, middleware.After)
}

func getValueByKey(rv reflect.Value, field string) (any, error) {
	switch rv.Kind() {
	case reflect.Struct:
		f := rv.FieldByName(field)
		if !f.IsValid() {
			return "", fmt.Errorf("field %s not found in struct", field)
		}
		return f.Interface(), nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String && rv.Type().Key().Kind() != reflect.Interface {
			return "", fmt.Errorf("expected string or interface key, got %s", rv.Type().Key().Kind())
		}
		vv := rv.MapIndex(reflect.ValueOf(field))
		if !vv.IsValid() {
			return "", fmt.Errorf("key %s not found in map", field)
		}
		return vv.Interface(), nil
	default:
		return "", fmt.Errorf("expected struct or string keyied map, got %s", rv.Kind())
	}
}

func convertStringSliceToAnySlice(entries []string) []any {
	retval := make([]any, len(entries))
	for i, entry := range entries {
		retval[i] = entry
	}
	return retval
}

func getActualValueOf(rv reflect.Value) reflect.Value {
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}
	return rv
}

// isValidEnvName reports whether s is a valid environment variable name
// ([A-Za-z_][A-Za-z0-9_]*), used to distinguish env NAME=VALUE operands from the
// command to run.
func isValidEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// errRetriesExhausted is returned by superviseWorkload when the workload has
// been restarted respawnMaxRetries times without staying up long enough.
var errRetriesExhausted = errors.New("workload respawn retries exhausted")

// healthState is the tri-state workload health tracked by the healthchecker.
type healthState int

const (
	healthUnknown healthState = iota
	healthHealthy
	healthUnhealthy
)

func (h healthState) String() string {
	switch h {
	case healthHealthy:
		return "healthy"
	case healthUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// harnessState is the single object shared between the supervisor, the
// healthchecker and the control socket. All access goes through the methods
// below, which hold mu.
type harnessState struct {
	mu sync.RWMutex

	startedAt         time.Time
	workloadUp        bool
	workloadPID       int
	workloadStartedAt time.Time
	lastExitCode      int
	lastExitErr       string
	respawnCount      int
	maxRetries        int
	currentBackoff    time.Duration

	health          healthState
	consecutiveOK   int
	consecutiveFail int
	lastProbeAt     time.Time
	lastProbeErr    string
}

func newHarnessState(maxRetries int) *harnessState {
	return &harnessState{startedAt: time.Now(), maxRetries: maxRetries}
}

func (st *harnessState) setWorkloadUp(pid int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.workloadUp = true
	st.workloadPID = pid
	st.workloadStartedAt = time.Now()
	st.currentBackoff = 0
}

func (st *harnessState) setWorkloadDown(code int, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.workloadUp = false
	st.workloadPID = 0
	st.lastExitCode = code
	st.lastExitErr = errString(err)
}

func (st *harnessState) incRespawn(next time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.respawnCount++
	st.currentBackoff = next
}

func (st *harnessState) resetRespawn() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.currentBackoff = 0
}

func (st *harnessState) currentHealth() healthState {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.health
}

func (st *harnessState) setHealth(hs healthState, ok, fail int, probeErr error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.health = hs
	st.consecutiveOK = ok
	st.consecutiveFail = fail
	st.lastProbeAt = time.Now()
	st.lastProbeErr = errString(probeErr)
}

// stateSnapshot is a plain, JSON-serialisable copy of harnessState served by
// the control socket and decoded by the health-probe client.
type stateSnapshot struct {
	Harness struct {
		Running       bool      `json:"running"`
		StartedAt     time.Time `json:"started_at"`
		UptimeSeconds float64   `json:"uptime_seconds"`
	} `json:"harness"`
	Workload struct {
		Up                    bool      `json:"up"`
		PID                   int       `json:"pid"`
		StartedAt             time.Time `json:"started_at"`
		UptimeSeconds         float64   `json:"uptime_seconds"`
		RespawnCount          int       `json:"respawn_count"`
		CurrentBackoffSeconds float64   `json:"current_backoff_seconds"`
		MaxRetries            int       `json:"max_retries"`
		LastExitCode          int       `json:"last_exit_code"`
		LastExitError         string    `json:"last_exit_error"`
	} `json:"workload"`
	Health struct {
		State           string    `json:"state"`
		ConsecutiveOK   int       `json:"consecutive_ok"`
		ConsecutiveFail int       `json:"consecutive_fail"`
		LastProbeAt     time.Time `json:"last_probe_at"`
		LastProbeError  string    `json:"last_probe_error"`
	} `json:"health"`
}

func (st *harnessState) snapshot() stateSnapshot {
	st.mu.RLock()
	defer st.mu.RUnlock()
	now := time.Now()
	var s stateSnapshot
	s.Harness.Running = true
	s.Harness.StartedAt = st.startedAt
	s.Harness.UptimeSeconds = now.Sub(st.startedAt).Seconds()
	s.Workload.Up = st.workloadUp
	s.Workload.PID = st.workloadPID
	s.Workload.StartedAt = st.workloadStartedAt
	if st.workloadUp && !st.workloadStartedAt.IsZero() {
		s.Workload.UptimeSeconds = now.Sub(st.workloadStartedAt).Seconds()
	}
	s.Workload.RespawnCount = st.respawnCount
	s.Workload.CurrentBackoffSeconds = st.currentBackoff.Seconds()
	s.Workload.MaxRetries = st.maxRetries
	s.Workload.LastExitCode = st.lastExitCode
	s.Workload.LastExitError = st.lastExitErr
	s.Health.State = st.health.String()
	s.Health.ConsecutiveOK = st.consecutiveOK
	s.Health.ConsecutiveFail = st.consecutiveFail
	s.Health.LastProbeAt = st.lastProbeAt
	s.Health.LastProbeError = st.lastProbeErr
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// exitCodeOf extracts the process exit code from a cmd.Wait() error. It returns
// 0 on success and -1 for a non-exit error (e.g. failure to start).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// healthProber probes the workload once and returns nil when it is healthy.
type healthProber func(ctx context.Context, target string, timeout time.Duration) error

var healthcheckers = map[string]healthProber{
	"http":  probeHTTP,
	"https": probeHTTP,
	"tcp":   probeTCP,
	"grpc":  probeGRPC,
}

func probeHTTP(ctx context.Context, target string, timeout time.Duration) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func probeTCP(ctx context.Context, target string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeGRPC performs a minimal grpc.health.v1.Health/Check over plaintext
// HTTP/2 (h2c) without pulling in the full gRPC runtime. It hand-encodes the
// HealthCheckRequest and decodes the HealthCheckResponse from the length-prefixed
// gRPC message framing.
func probeGRPC(ctx context.Context, target string, timeout time.Duration) error {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}

	// HealthCheckRequest{ string service = 1 }
	var msg []byte
	if healthcheckGRPCService != "" {
		svc := []byte(healthcheckGRPCService)
		msg = append(msg, 0x0A) // field 1, wire type 2 (length-delimited)
		msg = binary.AppendUvarint(msg, uint64(len(svc)))
		msg = append(msg, svc...)
	}
	frame := make([]byte, 5+len(msg))
	frame[0] = 0 // uncompressed
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(msg)))
	copy(frame[5:], msg)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+target+"/grpc.health.v1.Health/Check", bytes.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("TE", "trailers")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("grpc health check request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read grpc response: %w", err)
	}
	grpcStatus := resp.Trailer.Get("Grpc-Status")
	if grpcStatus == "" {
		grpcStatus = resp.Header.Get("Grpc-Status")
	}
	if grpcStatus != "" && grpcStatus != "0" {
		msg := resp.Trailer.Get("Grpc-Message")
		if msg == "" {
			msg = resp.Header.Get("Grpc-Message")
		}
		return fmt.Errorf("grpc health check returned status %s: %s", grpcStatus, msg)
	}
	status, err := parseGRPCHealthStatus(body)
	if err != nil {
		return err
	}
	const serving = 1
	if status != serving {
		return fmt.Errorf("grpc health status is not SERVING (got %d)", status)
	}
	return nil
}

// parseGRPCHealthStatus decodes HealthCheckResponse{ ServingStatus status = 1 }
// from a single length-prefixed gRPC frame.
func parseGRPCHealthStatus(frame []byte) (int, error) {
	if len(frame) < 5 {
		return 0, fmt.Errorf("grpc response too short")
	}
	n := binary.BigEndian.Uint32(frame[1:5])
	if len(frame) < 5+int(n) {
		return 0, fmt.Errorf("grpc response truncated")
	}
	msg := frame[5 : 5+n]
	for i := 0; i < len(msg); {
		tag, tl := binary.Uvarint(msg[i:])
		if tl <= 0 {
			return 0, fmt.Errorf("invalid protobuf tag")
		}
		i += tl
		field := tag >> 3
		switch tag & 0x7 {
		case 0: // varint
			v, vl := binary.Uvarint(msg[i:])
			if vl <= 0 {
				return 0, fmt.Errorf("invalid varint")
			}
			i += vl
			if field == 1 {
				return int(v), nil
			}
		case 1: // 64-bit
			i += 8
		case 2: // length-delimited
			l, ll := binary.Uvarint(msg[i:])
			if ll <= 0 {
				return 0, fmt.Errorf("invalid length")
			}
			i += ll + int(l)
		case 5: // 32-bit
			i += 4
		default:
			return 0, fmt.Errorf("unsupported wire type")
		}
	}
	return 0, nil // no status field => UNKNOWN
}

// runHealthcheck probes the workload periodically and records the result in st.
// When restartCh is non-nil (healthcheck-action=restart), a sustained-unhealthy
// transition sends one restart request to the supervisor.
func runHealthcheck(ctx context.Context, logger *slog.Logger, st *harnessState, prober healthProber, target string, restartCh chan<- struct{}) error {
	if healthcheckStartPeriod > 0 {
		logger.Info("healthcheck start period", slog.Duration("duration", healthcheckStartPeriod))
		select {
		case <-time.After(healthcheckStartPeriod):
		case <-ctx.Done():
			return nil
		}
	}
	logger.Info("healthcheck started", slog.String("type", healthcheckType), slog.String("target", target), slog.Duration("interval", healthcheckInterval))
	ticker := time.NewTicker(healthcheckInterval)
	defer ticker.Stop()
	ok, fail := 0, 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		probeCtx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
		err := prober(probeCtx, target, healthcheckTimeout)
		cancel()

		prev := st.currentHealth()
		hs := prev
		if err == nil {
			ok++
			fail = 0
			if ok >= healthcheckHealthyThreshold {
				hs = healthHealthy
			}
			st.setHealth(hs, ok, fail, nil)
			if hs == healthHealthy && prev != healthHealthy {
				logger.Info("workload became healthy")
			}
		} else {
			fail++
			ok = 0
			if fail >= healthcheckUnhealthyThreshold {
				hs = healthUnhealthy
			}
			st.setHealth(hs, ok, fail, err)
			if hs == healthUnhealthy && prev != healthUnhealthy {
				logger.Warn("workload became unhealthy", slog.String("err", err.Error()), slog.Int("consecutive_fail", fail))
			}
			if hs == healthUnhealthy && restartCh != nil {
				select {
				case restartCh <- struct{}{}:
					fail = 0 // reset so we don't spam restarts
					logger.Warn("requesting workload restart due to unhealthy status")
				default:
				}
			}
		}
	}
}

// superviseWorkload runs the workload and, when respawning is enabled, restarts
// it with exponential backoff up to respawnMaxRetries. It is signal-aware:
// cancelling ctx relays SIGTERM to the child (SIGKILL after shutdownGrace).
func superviseWorkload(ctx context.Context, logger *slog.Logger, st *harnessState, argv, env []string, restartCh <-chan struct{}) error {
	bo := &backoff.ExponentialBackOff{
		InitialInterval:     respawnInitialInterval,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          respawnMultiplier,
		MaxInterval:         respawnMaxInterval,
	}
	bo.Reset()
	attempts := 0
	for {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		// Graceful termination on ctx cancel: SIGTERM, then SIGKILL after WaitDelay.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = shutdownGrace

		startedAt := time.Now()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start command: %w", err)
		}
		pid := cmd.Process.Pid
		st.setWorkloadUp(pid)
		logger.Info("workload started", slog.Int("pid", pid), slog.Int("attempt", attempts))

		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		var waitErr error
		healthTriggered := false
		select {
		case <-ctx.Done():
			waitErr = <-waitCh
			st.setWorkloadDown(exitCodeOf(waitErr), waitErr)
			logger.Info("workload exited during shutdown", slog.String("err", errString(waitErr)))
			return nil
		case <-restartCh:
			healthTriggered = true
			logger.Info("terminating workload for health-triggered restart", slog.Int("pid", pid))
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case waitErr = <-waitCh:
			case <-time.After(shutdownGrace):
				_ = cmd.Process.Kill()
				waitErr = <-waitCh
			case <-ctx.Done():
				waitErr = <-waitCh
				st.setWorkloadDown(exitCodeOf(waitErr), waitErr)
				return nil
			}
		case waitErr = <-waitCh:
		}

		code := exitCodeOf(waitErr)
		st.setWorkloadDown(code, waitErr)
		uptime := time.Since(startedAt)
		logger.Info("workload exited", slog.Int("exit_code", code), slog.String("err", errString(waitErr)), slog.Duration("uptime", uptime))

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if uptime >= respawnResetAfter {
			bo.Reset()
			attempts = 0
			st.resetRespawn()
		}

		if !healthTriggered {
			if !respawnEnabled {
				if waitErr != nil {
					return fmt.Errorf("failed to run command: %w", waitErr)
				}
				return nil
			}
			if code == 0 && !respawnKeepAlive {
				return nil
			}
		}

		attempts++
		if respawnMaxRetries != 0 && attempts > respawnMaxRetries {
			return errRetriesExhausted
		}
		d := bo.NextBackOff()
		if d == backoff.Stop {
			return errRetriesExhausted
		}
		st.incRespawn(d)
		logger.Info("respawning workload", slog.Duration("backoff", d), slog.Int("attempt", attempts), slog.Int("max_retries", respawnMaxRetries))
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil
		}
	}
}

// serveControlSocket exposes GET /health over a unix-domain socket.
func serveControlSocket(ctx context.Context, logger *slog.Logger, st *harnessState, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale control socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("failed to listen on control socket: %w", err)
	}
	defer os.Remove(path)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st.snapshot())
	})
	srv := &http.Server{Handler: mux}
	logger.Info("control socket listening", slog.String("path", path))

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// runHealthProbe is the -health-probe client mode: it queries the control
// socket and returns nil (exit 0) when healthy, or an error (exit non-zero)
// otherwise. Intended for use as a container HEALTHCHECK CMD.
func runHealthProbe(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("-control-socket is required for -health-probe")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://harness/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to reach control socket: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read control socket response: %w", err)
	}
	var snap stateSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return fmt.Errorf("failed to decode control socket response: %w", err)
	}
	os.Stdout.Write(body)
	if n := len(body); n == 0 || body[n-1] != '\n' {
		os.Stdout.Write([]byte("\n"))
	}
	switch snap.Health.State {
	case "healthy":
		return nil
	case "unknown":
		// healthchecks disabled: fall back to whether the workload is running
		if snap.Workload.Up {
			return nil
		}
		return fmt.Errorf("workload is not up")
	default:
		return fmt.Errorf("workload health is %s", snap.Health.State)
	}
}

func doIt(ctx context.Context, logger *slog.Logger) error {
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if retryCount < 0 {
		return fmt.Errorf("retry count must be greater than or equal to 0")
	}
	if healthcheckType != "" {
		if _, ok := healthcheckers[strings.ToLower(healthcheckType)]; !ok {
			return fmt.Errorf("invalid healthcheck type: %s", healthcheckType)
		}
		if healthcheckTarget == "" {
			return fmt.Errorf("healthcheck target is required when a healthcheck type is set")
		}
		if healthcheckInterval <= 0 {
			return fmt.Errorf("healthcheck interval must be positive")
		}
		if healthcheckTimeout <= 0 || healthcheckTimeout >= healthcheckInterval {
			return fmt.Errorf("healthcheck timeout must be positive and less than the interval")
		}
		if healthcheckHealthyThreshold < 1 || healthcheckUnhealthyThreshold < 1 {
			return fmt.Errorf("healthcheck thresholds must be at least 1")
		}
	}
	switch strings.ToLower(healthcheckAction) {
	case "", "none", "restart":
	default:
		return fmt.Errorf("invalid healthcheck action: %s", healthcheckAction)
	}
	if strings.EqualFold(healthcheckAction, "restart") && healthcheckType == "" {
		return fmt.Errorf("healthcheck action 'restart' requires a healthcheck type")
	}
	if respawnEnabled {
		if respawnMultiplier <= 1 {
			return fmt.Errorf("respawn multiplier must be greater than 1")
		}
		if respawnMaxRetries < 0 {
			return fmt.Errorf("respawn max retries must be greater than or equal to 0")
		}
	}
	var preconditionFunc func(context.Context, *aws.Config, time.Duration, time.Duration) error
	if precondition != "" {
		var ok bool
		preconditionFunc, ok = preconditions[strings.ToLower(precondition)]
		if !ok {
			return fmt.Errorf("invalid precondition: %s", precondition)
		}
	}
	hsf := servicediscovery_types.HealthStatusFilter(healthStatus)
	i := slices.Index[[]servicediscovery_types.HealthStatusFilter](
		servicediscovery_types.HealthStatusFilterAll.Values(),
		hsf,
	)
	if i < 0 {
		return fmt.Errorf("invalid health status: %s", healthStatus)
	}
	cmdLine := flag.Args()
	if len(cmdLine) < 1 {
		return fmt.Errorf("command is required")
	}

	// env utility emulation: if the first token is literally "env", consume the
	// leading NAME=VALUE operands and set them as environment variables on the
	// command being run. Only the NAME=VALUE form is supported (no options). The
	// VALUE part is interpolated with the same template functions as the argv.
	type envAssignment struct {
		name      string
		valueTmpl string
	}
	var envAssignments []envAssignment
	if cmdLine[0] == "env" {
		i := 1
		for ; i < len(cmdLine); i++ {
			arg := cmdLine[i]
			eq := strings.IndexByte(arg, '=')
			if eq <= 0 || !isValidEnvName(arg[:eq]) {
				break
			}
			envAssignments = append(envAssignments, envAssignment{
				name:      arg[:eq],
				valueTmpl: arg[eq+1:],
			})
		}
		cmdLine = cmdLine[i:]
		if len(cmdLine) < 1 {
			return fmt.Errorf("command is required")
		}
	}

	var err error
	cmdLine[0], err = exec.LookPath(cmdLine[0])
	if err != nil {
		return fmt.Errorf("command not found: %s", cmdLine[0])
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	options := make([]func(*servicediscovery.Options), 0, 1)
	if cfg.BaseEndpoint != nil {
		options = append(options, servicediscovery.WithAPIOptions(addDisableEndpointPrefix))
	}
	svc := servicediscovery.NewFromConfig(
		cfg,
		options...,
	)
	{
		loggerOpts := []any{
			slog.String("aws_region", cfg.Region),
			slog.String("namespace", namespace),
			slog.String("health_status", healthStatus),
			slog.Int("retries", retryCount),
			slog.Bool("no_fail", noFail),
		}
		if cfg.BaseEndpoint != nil {
			loggerOpts = append(loggerOpts, slog.String("aws_endpoint", *cfg.BaseEndpoint))
		}
		logger.Info(
			"service discovery will be performed",
			loggerOpts...,
		)
	}
	sd := &serviceDiscovery{
		svc:       svc,
		namespace: namespace,
		hsf:       hsf,
		maxTries:  retryCount + 1,
	}

	ifAddrCache := make(map[string]string)
	funcMap := template.FuncMap{
		"instances": func(service string) ([]entry, error) {
			entries, err := sd.do(ctx, service)
			if err != nil {
				if !noFail {
					return nil, err
				}
			}
			return entries, nil
		},
		"exclude": func(addr string, entries []entry) ([]entry, error) {
			retval := make([]entry, 0, len(entries))
			for _, entry := range entries {
				if entry.IPv4Addr == addr || entry.IPv6Addr == addr {
					continue
				}
				retval = append(retval, entry)
			}
			return retval, nil
		},
		"extract": func(field string, entries any) ([]any, error) {
			fields := strings.Split(field, ",")
			for i, field := range fields {
				fields[i] = strings.TrimSpace(field)
			}
			rv := getActualValueOf(reflect.ValueOf(entries))
			if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
				return nil, fmt.Errorf("expected array or slice, got %s", rv.Kind())
			}
			retval := make([]any, rv.Len())
			if len(fields) == 1 {
				for i := 0; i < rv.Len(); i++ {
					v, err := getValueByKey(getActualValueOf(rv.Index(i)), fields[0])
					if err != nil {
						return nil, fmt.Errorf("failed to get value by key %s: %w", field, err)
					}
					retval[i] = v
				}
			} else {
				for i := 0; i < rv.Len(); i++ {
					vv := make([]any, len(fields))
					for j, field := range fields {
						v, err := getValueByKey(getActualValueOf(rv.Index(i)), field)
						if err != nil {
							return nil, fmt.Errorf("failed to get value by key %s: %w", field, err)
						}
						vv[j] = v
					}
					retval[i] = vv
				}
			}
			return retval, nil
		},
		"mapprintf": func(format string, entries any) ([]string, error) {
			switch entries := entries.(type) {
			case []string:
				retval := make([]string, len(entries))
				for i, entry := range entries {
					retval[i] = fmt.Sprintf(format, entry)
				}
				return retval, nil
			case [][]any:
				retval := make([]string, len(entries))
				for i, entry := range entries {
					retval[i] = fmt.Sprintf(format, entry...)
				}
				return retval, nil
			case [][]string:
				retval := make([]string, len(entries))
				for i, entry := range entries {
					retval[i] = fmt.Sprintf(format, convertStringSliceToAnySlice(entry)...)
				}
				return retval, nil
			default:
				rentries := getActualValueOf(reflect.ValueOf(entries))
				if rentries.Kind() != reflect.Slice && rentries.Kind() != reflect.Array {
					return nil, fmt.Errorf("expected []any, or [#]any, got %s", rentries.Kind())
				}
				retval := make([]string, rentries.Len())
				for i := range retval {
					rentry := getActualValueOf(rentries.Index(i))
					if rentry.Kind() != reflect.Slice && rentry.Kind() != reflect.Array {
						retval[i] = fmt.Sprintf(format, rentry.Interface())
					} else {
						args := make([]any, rentry.Len())
						for j := 0; j < rentry.Len(); j++ {
							args[j] = rentry.Index(j).Interface()
						}
						retval[i] = fmt.Sprintf(format, args...)
					}
				}
				return retval, nil
			}
		},
		"join": func(sep string, entries any) (string, error) {
			switch entries := entries.(type) {
			case []string:
				return strings.Join(entries, sep), nil
			case []any:
				var result strings.Builder
				for i, entry := range entries {
					if i > 0 {
						result.WriteString(sep)
					}
					result.WriteString(getActualValueOf(reflect.ValueOf(entry)).String())
				}
				return result.String(), nil
			default:
				return "", fmt.Errorf("expected []string or []any, got %s", reflect.TypeOf(entries))
			}
		},
		"ifaddr": func(cidr string) (string, error) {
			if addrStr, ok := ifAddrCache[cidr]; ok {
				return addrStr, nil
			}
			pfx, err := netip.ParsePrefix(cidr)
			if err != nil {
				return "", fmt.Errorf("failed to parse CIDR: %w", err)
			}
			ifs, err := net.Interfaces()
			if err != nil {
				return "", fmt.Errorf("failed to get interfaces: %w", err)
			}
			for _, if_ := range ifs {
				addrs, err := if_.Addrs()
				if err != nil {
					return "", fmt.Errorf("failed to get interface addresses: %w", err)
				}
				if if_.Flags&net.FlagUp == 0 {
					continue
				}
				if if_.Flags&net.FlagPointToPoint != 0 {
					continue
				}
				if if_.Flags&net.FlagLoopback != 0 {
					continue
				}
				for _, addr := range addrs {
					ip, err := netip.ParsePrefix(addr.String())
					if err != nil {
						return "", fmt.Errorf("failed to parse address: %w", err)
					}
					if pfx.Contains(ip.Addr()) {
						addrStr := ip.Addr().String()
						ifAddrCache[cidr] = addrStr
						return addrStr, nil
					}
				}
			}
			return "", fmt.Errorf("no applicable interfaces found")
		},
	}

	cmdLineT := make([]*template.Template, len(cmdLine)-1)
	for i, arg := range cmdLine[1:] {
		t, err := template.New(strconv.Itoa(i)).Funcs(funcMap).Parse(arg)
		if err != nil {
			return fmt.Errorf("failed to parse command line: %w", err)
		}
		cmdLineT[i] = t
	}

	envValueT := make([]*template.Template, len(envAssignments))
	for i, a := range envAssignments {
		t, err := template.New("env" + strconv.Itoa(i)).Funcs(funcMap).Parse(a.valueTmpl)
		if err != nil {
			return fmt.Errorf("failed to parse env value for %s: %w", a.name, err)
		}
		envValueT[i] = t
	}

	if preconditionFunc != nil {
		logger.Info("checking precondition", slog.String("precondition", precondition))
		if err := preconditionFunc(ctx, &cfg, 3*time.Second, preconditionCheckTimeout); err != nil {
			return fmt.Errorf("precondition check failed: %w", err)
		}
	}

	delay := executionDelayJitterUnit * time.Duration(
		rand.Int64N(
			int64(executionDelayJitter)/int64(executionDelayJitterUnit)+1,
		),
	)
	logger.Info("delaying execution", slog.Duration("delay", delay))
	time.Sleep(delay)

	renderedCmdLine := make([]string, len(cmdLine))
	renderedCmdLine[0] = cmdLine[0]
	for i, t := range cmdLineT {
		var buf bytes.Buffer
		if err := t.Execute(&buf, nil); err != nil {
			return fmt.Errorf("failed to execute template: %w", err)
		}
		renderedCmdLine[i+1] = buf.String()
	}

	extraEnv := make([]string, len(envValueT))
	envNames := make([]string, len(envValueT))
	for i, t := range envValueT {
		var buf bytes.Buffer
		if err := t.Execute(&buf, nil); err != nil {
			return fmt.Errorf("failed to execute env template for %s: %w", envAssignments[i].name, err)
		}
		extraEnv[i] = envAssignments[i].name + "=" + buf.String()
		envNames[i] = envAssignments[i].name
	}

	logger.Info("running", slog.Any("argv", renderedCmdLine), slog.Any("env", envNames))

	argv := renderedCmdLine
	env := append(os.Environ(), extraEnv...)

	// Orchestrate the supervisor plus the optional background legs (healthcheck
	// and control socket) under a shared, cancellable context. Each leg cancels
	// the context on return, so a clean supervisor exit also tears down the
	// background legs (which errgroup.WithContext alone would not do).
	st := newHarnessState(respawnMaxRetries)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var restartCh chan struct{}
	if healthcheckType != "" && strings.EqualFold(healthcheckAction, "restart") {
		restartCh = make(chan struct{}, 1)
	}

	var g errgroup.Group
	if healthcheckType != "" {
		prober := healthcheckers[strings.ToLower(healthcheckType)]
		g.Go(func() error {
			defer cancel()
			return runHealthcheck(gctx, logger, st, prober, healthcheckTarget, restartCh)
		})
	}
	if controlSocket != "" {
		g.Go(func() error {
			defer cancel()
			return serveControlSocket(gctx, logger, st, controlSocket)
		})
	}
	g.Go(func() error {
		defer cancel()
		return superviseWorkload(gctx, logger, st, argv, env, restartCh)
	})
	return g.Wait()
}

func main() {
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if healthProbe {
		// Probe-client mode: no harness, no workload. Exit code is the health signal.
		if err := runHealthProbe(ctx, controlSocket); err != nil {
			logger.Error("unhealthy", slog.Any("err", err))
			os.Exit(1)
		}
		return
	}

	err := doIt(ctx, logger)
	if errors.Is(err, errRetriesExhausted) {
		logger.Error("workload respawn retries exhausted", slog.Any("err", err))
		os.Exit(1)
	}
	if err != nil {
		logger.Error("failed", slog.Any("err", err))
		os.Exit(1)
	}
}
