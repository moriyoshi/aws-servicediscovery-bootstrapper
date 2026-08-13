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
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.starlark.net/starlark"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"

	"github.com/moriyoshi/muster/internal/provider"
	_ "github.com/moriyoshi/muster/internal/providers"
)

var namespace string

// Respawn/supervision and healthcheck settings are configured exclusively from
// the script via the respawn() and healthcheck() builtins (see runtimeConfig).

// control socket
var controlSocket string
var healthProbe bool

// starlark engine
var scriptPath string
var providerName string
var providerOpts = optFlags{}
var providerHelpFlag bool
var kvStoreName string
var kvKeyPrefix string
var kvCreate bool
var allowRun bool

func init() {
	flag.StringVar(&namespace, "namespace", "", "The namespace of the instance to be listed")

	flag.StringVar(&controlSocket, "control-socket", "", "Path to a unix-domain control socket serving GET /health (empty = disabled)")
	flag.BoolVar(&healthProbe, "health-probe", false, "Run as a health-probe client against -control-socket and exit 0 (healthy) or non-zero; for use as a container HEALTHCHECK CMD")

	flag.StringVar(&scriptPath, "script", "", "Path to the Starlark script that resolves the workload and defines lifecycle hooks (required)")
	flag.StringVar(&providerName, "provider", "", "Cloud provider to use (empty = $MUSTER_PROVIDER, then autodetect)")
	flag.Var(providerOpts, "provider-opt", "Provider-specific option, k=v (repeatable); see -provider-help")
	flag.BoolVar(&providerHelpFlag, "provider-help", false, "List the compiled-in providers and their -provider-opt keys, then exit")
	flag.StringVar(&kvStoreName, "kv-store", "", "Name of the provider-side store backing the kv_* builtins -- a DynamoDB table on AWS (empty = kv disabled)")
	flag.StringVar(&kvKeyPrefix, "kv-key-prefix", "", "Prefix applied to all kv_* keys, to namespace multiple clusters on one store")
	flag.BoolVar(&kvCreate, "kv-create", false, "Create the kv store with provider defaults if it does not exist")
	flag.BoolVar(&allowRun, "allow-run", false, "Expose the run() builtin to scripts (arbitrary command execution)")

	// Registered only to report their replacement; see renamedFlags.
	for old := range renamedFlags {
		flag.String(old, "", "removed; see -"+renamedFlags[old])
	}
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

// harnessState is the registry shared between the supervisors and the control
// socket: harness-level info plus one workloadState per spawn() call. Access goes
// through methods that hold mu, so a snapshot sees a consistent cross-workload
// view even while several supervisors update concurrently.
type harnessState struct {
	mu sync.RWMutex

	startedAt time.Time
	workloads []*workloadState
}

// workloadState is the per-spawn() process + health state. Its mutators lock the
// parent registry's mu; a supervisor only ever touches its own entry.
type workloadState struct {
	st   *harnessState
	name string

	up             bool
	pid            int
	startedAt      time.Time
	lastExitCode   int
	lastExitErr    string
	respawnCount   int
	maxRetries     int
	currentBackoff time.Duration

	health          healthState
	consecutiveOK   int
	consecutiveFail int
	lastProbeAt     time.Time
	lastProbeErr    string
}

func newHarnessState() *harnessState {
	return &harnessState{startedAt: time.Now()}
}

// register adds a workload entry and returns its handle. name labels it in the
// control-socket snapshot; an empty name is auto-assigned from its position.
func (st *harnessState) register(name string, maxRetries int) *workloadState {
	st.mu.Lock()
	defer st.mu.Unlock()
	if name == "" {
		name = fmt.Sprintf("workload-%d", len(st.workloads))
	}
	w := &workloadState{st: st, name: name, maxRetries: maxRetries}
	st.workloads = append(st.workloads, w)
	return w
}

func (w *workloadState) setUp(pid int) {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	w.up = true
	w.pid = pid
	w.startedAt = time.Now()
	w.currentBackoff = 0
}

func (w *workloadState) setDown(code int, err error) {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	w.up = false
	w.pid = 0
	w.lastExitCode = code
	w.lastExitErr = errString(err)
}

func (w *workloadState) incRespawn(next time.Duration) {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	w.respawnCount++
	w.currentBackoff = next
}

func (w *workloadState) resetRespawn() {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	w.currentBackoff = 0
}

func (w *workloadState) setHealth(hs healthState, ok, fail int, probeErr error) {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	w.health = hs
	w.consecutiveOK = ok
	w.consecutiveFail = fail
	w.lastProbeAt = time.Now()
	w.lastProbeErr = errString(probeErr)
}

// stateSnapshot is the JSON served by the control socket and decoded by the
// health-probe client. workloads holds one entry per spawn().
type stateSnapshot struct {
	Harness struct {
		Running       bool      `json:"running"`
		StartedAt     time.Time `json:"started_at"`
		UptimeSeconds float64   `json:"uptime_seconds"`
	} `json:"harness"`
	Workloads []workloadSnapshot `json:"workloads"`
}

type workloadSnapshot struct {
	Name                  string    `json:"name"`
	Up                    bool      `json:"up"`
	PID                   int       `json:"pid"`
	StartedAt             time.Time `json:"started_at"`
	UptimeSeconds         float64   `json:"uptime_seconds"`
	RespawnCount          int       `json:"respawn_count"`
	CurrentBackoffSeconds float64   `json:"current_backoff_seconds"`
	MaxRetries            int       `json:"max_retries"`
	LastExitCode          int       `json:"last_exit_code"`
	LastExitError         string    `json:"last_exit_error"`
	Health                struct {
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
	s.Workloads = make([]workloadSnapshot, 0, len(st.workloads))
	for _, w := range st.workloads {
		var ws workloadSnapshot
		ws.Name = w.name
		ws.Up = w.up
		ws.PID = w.pid
		ws.StartedAt = w.startedAt
		if w.up && !w.startedAt.IsZero() {
			ws.UptimeSeconds = now.Sub(w.startedAt).Seconds()
		}
		ws.RespawnCount = w.respawnCount
		ws.CurrentBackoffSeconds = w.currentBackoff.Seconds()
		ws.MaxRetries = w.maxRetries
		ws.LastExitCode = w.lastExitCode
		ws.LastExitError = w.lastExitErr
		ws.Health.State = w.health.String()
		ws.Health.ConsecutiveOK = w.consecutiveOK
		ws.Health.ConsecutiveFail = w.consecutiveFail
		ws.Health.LastProbeAt = w.lastProbeAt
		ws.Health.LastProbeError = w.lastProbeErr
		s.Workloads = append(s.Workloads, ws)
	}
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

// probeHTTP, probeTCP and probeGRPCService back the http_ok/tcp_ok/grpc_ok check
// factories, which a script composes into readiness/liveness via poll().
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

// probeGRPCService performs a minimal grpc.health.v1.Health/Check over plaintext
// HTTP/2 (h2c) without pulling in the full gRPC runtime. It hand-encodes the
// HealthCheckRequest and decodes the HealthCheckResponse from the length-prefixed
// gRPC message framing.
func probeGRPCService(ctx context.Context, target, service string, timeout time.Duration) error {
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
	if service != "" {
		svc := []byte(service)
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
	// The container is healthy only when every workload is: a workload with a
	// health probe must be "healthy"; one without a probe ("unknown") falls back
	// to being up. Any unhealthy or down workload fails the probe.
	if len(snap.Workloads) == 0 {
		return fmt.Errorf("no workloads running")
	}
	for _, w := range snap.Workloads {
		switch w.Health.State {
		case "healthy":
			// ok
		case "unknown":
			if !w.Up {
				return fmt.Errorf("workload %s is not up", w.Name)
			}
		default:
			return fmt.Errorf("workload %s health is %s", w.Name, w.Health.State)
		}
	}
	return nil
}

// openProvider selects the provider and resolves every capability from it. Each
// capability keeps whatever error it failed with, so the builtin that needs one
// can say why at the point of use rather than the harness refusing to start over
// something the script may never touch.
func openProvider(ctx context.Context, logger *slog.Logger) (provider.Provider, *engineDeps, error) {
	factory, err := provider.Select(providerName)
	if err != nil {
		return nil, nil, err
	}
	p, err := factory.Open(ctx, provider.Config{
		Logger:    logger,
		Namespace: namespace,
		KVStore:   kvStoreName,
		KVPrefix:  kvKeyPrefix,
		Options:   providerOpts,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("provider %q: %w", factory.Name(), err)
	}

	// Identity first: the kv lease owner is derived from it, and Renew
	// conditions on that owner, so a store built before identity resolved would
	// hand out leases nobody can renew.
	self, err := p.Self(ctx)
	if err != nil {
		// A provider that has no notion of identity is not a problem; a
		// provider that has one and could not read it is, because the fallbacks
		// it feeds (the lease owner, the replica-status target) get quietly
		// worse. Only the second deserves a warning.
		level := slog.LevelWarn
		if errors.Is(err, provider.ErrUnsupported) {
			level = slog.LevelDebug
		}
		logger.Log(ctx, level, "instance identity unavailable", slog.String("err", err.Error()))
		self = nil
	}

	kv := capability(p.KV(ctx))
	if kvCreate {
		if err := provisionKV(ctx, p, kv); err != nil {
			return nil, nil, err
		}
	}

	deps := &engineDeps{
		logger:   logger,
		disc:     capability(p.Discovery(ctx)),
		kv:       kv,
		fleet:    capability(p.Fleet(ctx)),
		reg:      capability(p.Registrar(ctx)),
		self:     self,
		provider: p.Name(),
		command:  flag.Args(),
		allowRun: allowRun,
		st:       newHarnessState(),
		ifCache:  map[string]string{},
	}
	return p, deps, nil
}

// provisionKV honours -kv-create. Unlike the other capability failures this one
// is fatal: the operator asked for the store to exist, so carrying on to fail
// later inside a kv_* builtin would only obscure the reason.
func provisionKV(ctx context.Context, p provider.Provider, kv optional[provider.KVStore]) error {
	store, err := kv.require("kv store")
	if err != nil {
		return fmt.Errorf("-kv-create: %w", err)
	}
	prov, ok := store.(provider.Provisioner)
	if !ok {
		return fmt.Errorf("-kv-create: provider %q cannot create its kv store; provision it out of band", p.Name())
	}
	if err := prov.Provision(ctx); err != nil {
		return fmt.Errorf("failed to ensure kv store: %w", err)
	}
	return nil
}

func doIt(ctx context.Context, logger *slog.Logger) error {
	// respawn/healthcheck settings come from the script's spawn() arguments;
	// preconditions are folded into pre_start() via the replica-status builtin.
	// The trailing args after `--` are optional; they are exposed to the script
	// as the COMMAND global so a script can transform a base command.
	if scriptPath == "" {
		return fmt.Errorf("-script is required")
	}

	prov, deps, err := openProvider(ctx, logger)
	if err != nil {
		return err
	}
	defer prov.Close()
	st := deps.st

	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eng, err := loadScript(gctx, scriptPath, deps)
	if err != nil {
		return err
	}

	// The control socket runs concurrently under gctx, reading the shared state.
	var g errgroup.Group
	if controlSocket != "" {
		g.Go(func() error { return serveControlSocket(gctx, logger, st, controlSocket) })
	}

	// main() drives and returns the workload promise; the harness awaits it and
	// delivers SIGTERM/SIGINT to it via signal() (graceful stop), falling back to
	// cancel() for a non-signallable promise. A hard deadline caps teardown.
	p, err := eng.callMain(gctx)
	if err != nil {
		cancel()
		_ = g.Wait()
		return err
	}

	var hardCh <-chan time.Time
	shutdown := gctx.Done()
	var awaitErr error
loop:
	for {
		select {
		case <-p.doneCh:
			awaitErr = p.err
			break loop
		case <-shutdown:
			if p.signallable {
				p.doSignal(starlark.None)
			} else {
				p.doCancel()
			}
			hardCh = time.After(hardShutdownGrace)
			shutdown = nil
		case <-hardCh:
			awaitErr = fmt.Errorf("workload did not stop within %s of shutdown signal", hardShutdownGrace)
			break loop
		}
	}
	cancel()
	_ = g.Wait()
	return awaitErr
}

// hardShutdownGrace caps how long the harness waits for graceful teardown after
// delivering the shutdown signal to main()'s promise.
const hardShutdownGrace = 60 * time.Second

func main() {
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Anything not passed on the command line can come from MUSTER_<FLAG>. See
	// envflags.go for why a container entrypoint needs both channels.
	if err := applyEnvDefaults(flag.CommandLine, os.LookupEnv); err != nil {
		logger.Error("invalid configuration from the environment", slog.Any("err", err))
		os.Exit(2)
	}

	if providerHelpFlag {
		fmt.Print(providerHelp())
		return
	}
	// Checked after parsing rather than left to flag itself: "flag provided but
	// not defined" cannot name the replacement, and these two live in a task
	// definition that can lag the image.
	if err := checkRenamedFlags(func(name string) bool {
		found := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == name {
				found = true
			}
		})
		return found
	}); err != nil {
		logger.Error("removed flag", slog.Any("err", err))
		os.Exit(2)
	}

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
