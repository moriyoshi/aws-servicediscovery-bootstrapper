//go:build e2e_tikv

package tikv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sdtypes "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
)

const (
	// envEnabled gates the whole thing: this test provisions billable AWS
	// infrastructure, so it never runs by accident.
	envEnabled = "MUSTER_E2E_TIKV"
	// envProvision=0 asserts against a stack that is already up (`make up`).
	envProvision = "MUSTER_E2E_TIKV_PROVISION"
	// envKeep=1 leaves the stack running after a failure, for post-mortems.
	envKeep = "MUSTER_E2E_TIKV_KEEP"
)

// --- process plumbing ------------------------------------------------------

// lineWriter forwards a child process's output to t.Log a line at a time, so
// terraform and docker progress stays readable under `go test -v`.
type lineWriter struct {
	t      *testing.T
	prefix string
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.Reset()
			w.buf.WriteString(line) // keep the partial line for the next write
			break
		}
		w.t.Logf("%s%s", w.prefix, strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if rest := strings.TrimSpace(w.buf.String()); rest != "" {
		w.t.Logf("%s%s", w.prefix, rest)
	}
	w.buf.Reset()
}

// run executes a command in the e2e/tikv directory with its output tee'd into
// the test log. It deliberately has no timeout of its own: `go test -timeout`
// is the single place that bounds the run.
func run(t *testing.T, name string, args ...string) error {
	t.Helper()
	t.Logf("$ %s %s", name, strings.Join(args, " "))

	out := &lineWriter{t: t, prefix: "  | "}
	defer out.flush()

	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	return cmd.Run()
}

func makeTarget(t *testing.T, target string) error {
	t.Helper()
	return run(t, "make", target)
}

// --- terraform outputs -----------------------------------------------------

type tfOutputs map[string]struct {
	Value json.RawMessage `json:"value"`
}

func readTerraformOutputs(t *testing.T) tfOutputs {
	t.Helper()

	bin := os.Getenv("TERRAFORM")
	if bin == "" {
		bin = "terraform"
	}
	cmd := exec.Command(bin, "-chdir=terraform", "output", "-json")
	cmd.Stderr = &lineWriter{t: t, prefix: "  | "}
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("terraform output: %v (has `make up` run?)", err)
	}

	var out tfOutputs
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse terraform output: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("terraform reported no outputs; the stack is not provisioned")
	}
	return out
}

func (o tfOutputs) str(t *testing.T, key string) string {
	t.Helper()
	var v string
	o.decode(t, key, &v)
	return v
}

func (o tfOutputs) int(t *testing.T, key string) int {
	t.Helper()
	var v int
	o.decode(t, key, &v)
	return v
}

func (o tfOutputs) decode(t *testing.T, key string, into any) {
	t.Helper()
	entry, ok := o[key]
	if !ok {
		t.Fatalf("terraform output %q is missing", key)
	}
	if err := json.Unmarshal(entry.Value, into); err != nil {
		t.Fatalf("terraform output %q: %v", key, err)
	}
}

// --- the stack under test --------------------------------------------------

type stack struct {
	region       string
	cluster      string
	pdService    string
	tikvService  string
	namespace    string
	pdDiscovery  string
	tikvDiscover string
	kvTable      string
	pdCount      int
	tikvCount    int

	ecs *ecs.Client
	sd  *servicediscovery.Client
	ddb *dynamodb.Client
	pd  *pdClient
}

// setupStack provisions the stack unless told not to, then wires up the AWS
// clients and the PD HTTP client the assertions run against.
func setupStack(t *testing.T) *stack {
	t.Helper()

	if os.Getenv(envEnabled) != "1" {
		t.Skipf("set %s=1 to run the TiKV-on-Fargate e2e test (it creates billable AWS resources)", envEnabled)
	}

	// The cluster is only reachable through ECS Exec, which the AWS CLI drives
	// via the session-manager-plugin; without it there is no way in at all.
	requireTools(t, "aws", "session-manager-plugin")

	if os.Getenv(envProvision) != "0" {
		requireTools(t, "make", "terraform", "docker")

		t.Cleanup(func() {
			if os.Getenv(envKeep) == "1" {
				t.Logf("%s=1: leaving the stack up; run `make destroy` when you are done", envKeep)
				return
			}
			// Not t.Fatal: teardown failure must not mask the real result,
			// but it does have to be loud, since it costs money.
			if err := makeTarget(t, "destroy"); err != nil {
				t.Errorf("terraform destroy failed, the stack may still be running: %v", err)
			}
		})

		start := time.Now()
		if err := makeTarget(t, "up"); err != nil {
			t.Fatalf("provisioning failed after %s: %v", time.Since(start).Round(time.Second), err)
		}
		t.Logf("stack provisioned in %s", time.Since(start).Round(time.Second))
	}

	out := readTerraformOutputs(t)
	s := &stack{
		region:       out.str(t, "region"),
		cluster:      out.str(t, "ecs_cluster"),
		pdService:    out.str(t, "pd_service"),
		tikvService:  out.str(t, "tikv_service"),
		namespace:    out.str(t, "namespace_name"),
		pdDiscovery:  out.str(t, "pd_discovery_name"),
		tikvDiscover: out.str(t, "tikv_discovery_name"),
		kvTable:      out.str(t, "kv_table"),
		pdCount:      out.int(t, "pd_desired_count"),
		tikvCount:    out.int(t, "tikv_desired_count"),
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(s.region))
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	s.ecs = ecs.NewFromConfig(cfg)
	s.sd = servicediscovery.NewFromConfig(cfg)
	s.ddb = dynamodb.NewFromConfig(cfg)
	s.pd = &pdClient{cluster: s.cluster, region: s.region, port: out.int(t, "pd_client_port")}

	// Registered after the teardown hook, so LIFO runs it first: the logs have
	// to be read out before `terraform destroy` deletes the log groups.
	pdGroup, tikvGroup := out.str(t, "log_group_tikv_pd"), out.str(t, "log_group_tikv_node")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Log("test failed; dumping task logs before teardown")
		s.dumpLogs(t, pdGroup, time.Hour, 400)
		s.dumpLogs(t, tikvGroup, time.Hour, 200)
	})

	t.Logf("cluster=%s namespace=%s pd=%s (%d) tikv=%s (%d)",
		s.cluster, s.namespace, s.pdService, s.pdCount, s.tikvService, s.tikvCount)
	return s
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not on PATH; cannot provision the stack", name)
		}
	}
}

// --- PD client (over ECS Exec) ---------------------------------------------

// Nothing in this stack is reachable from outside the VPC, so PD is queried by
// running curl inside a task with `aws ecs execute-command`. That is not just a
// workaround for the missing load balancer: it addresses one named replica
// rather than whichever one a balancer happened to pick, which is what makes
// the split-brain check exhaustive instead of statistical.
//
// The payload is fenced with markers and the exit status echoed in band; see
// execout.go for why and for the parser.
type pdClient struct {
	cluster string
	region  string
	port    int
}

// get fetches a PD API path from one specific task and decodes it into `into`.
func (c *pdClient) get(ctx context.Context, task, path string, into any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path)

	// Single-quoted, and nothing inside needs a single quote of its own, so the
	// remote shell sees the script verbatim. $? is captured immediately: the
	// `echo` after it would otherwise overwrite curl's status.
	script := fmt.Sprintf("echo %s; curl -sS -f --max-time 10 %s; rc=$?; echo; echo %s=$rc; echo %s",
		execBegin, url, execStatus, execEnd)

	body, err := c.exec(ctx, task, "/bin/sh -c '"+script+"'")
	if err != nil {
		return fmt.Errorf("%s on %s: %w", path, shortTaskID(task), err)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s on %s: decode %q: %w", path, shortTaskID(task), truncate(string(body), 200), err)
	}
	return nil
}

// exec runs a command in a task and returns the fenced payload. Failures to
// establish the session at all are retried: the SSM agent takes a moment to
// connect after a task starts, and reports TargetNotConnectedException until
// it does.
func (c *pdClient) exec(ctx context.Context, task, command string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}

		cmd := exec.CommandContext(ctx, "aws", "ecs", "execute-command",
			"--region", c.region,
			"--cluster", c.cluster,
			"--task", task,
			"--container", "default",
			"--interactive",
			"--command", command)
		cmd.Stdin = nil // the plugin skips raw-mode setup when stdin is not a tty

		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		runErr := cmd.Run()

		body, status, ok := parseExecOutput(buf.String())
		if !ok {
			// No fence in the output: the session never carried our command.
			lastErr = fmt.Errorf("ecs execute-command: %v: %s", runErr, truncate(collapse(buf.String()), 300))
			continue
		}
		if status != 0 {
			// curl(1) exit codes: 7 refused, 22 HTTP >= 400, 28 timeout.
			return nil, fmt.Errorf("curl exited %d: %s", status, truncate(collapse(string(body)), 300))
		}
		return body, nil
	}
	return nil, lastErr
}

// PD API responses, trimmed to the fields the assertions use.

type pdMembers struct {
	Members []pdMember `json:"members"`
	Leader  pdMember   `json:"leader"`
}

type pdMember struct {
	Name       string   `json:"name"`
	MemberID   uint64   `json:"member_id"`
	ClientURLs []string `json:"client_urls"`
}

func (m pdMembers) names() []string {
	names := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		names = append(names, mem.Name)
	}
	return names
}

type pdHealthEntry struct {
	Name     string `json:"name"`
	MemberID uint64 `json:"member_id"`
	Health   bool   `json:"health"`
}

type pdClusterInfo struct {
	ID           uint64 `json:"id"`
	MaxPeerCount int    `json:"max_peer_count"`
}

type pdStores struct {
	Count  int `json:"count"`
	Stores []struct {
		Store struct {
			ID        uint64 `json:"id"`
			Address   string `json:"address"`
			StateName string `json:"state_name"`
		} `json:"store"`
	} `json:"stores"`
}

type pdRegions struct {
	Count   int `json:"count"`
	Regions []struct {
		ID    uint64 `json:"id"`
		Peers []struct {
			ID      uint64 `json:"id"`
			StoreID uint64 `json:"store_id"`
		} `json:"peers"`
		Leader struct {
			ID      uint64 `json:"id"`
			StoreID uint64 `json:"store_id"`
		} `json:"leader"`
	} `json:"regions"`
}

// --- polling ---------------------------------------------------------------

// errTerminal marks a failure that no amount of retrying can fix — an ECS
// deployment the circuit breaker has already given up on, say. Without it a
// dead cluster burns every subtest's full timeout in turn, which is how one
// broken run came to take 80 minutes to report a failure it knew about in 15.
var errTerminal = errors.New("not retryable")

// eventually retries fn until it succeeds, hits a terminal error, or runs out
// of time, logging each failure so a timeout says what it was still waiting for.
func eventually(t *testing.T, what string, timeout, interval time.Duration, fn func(context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	var last error
	for attempt := 1; ; attempt++ {
		last = fn(ctx)
		if last == nil {
			t.Logf("%s: ok after %s", what, time.Since(start).Round(time.Second))
			return
		}
		if errors.Is(last, errTerminal) {
			t.Fatalf("%s: gave up after %s: %v", what, time.Since(start).Round(time.Second), last)
		}
		if ctx.Err() != nil {
			break
		}
		if attempt%5 == 1 {
			t.Logf("%s: waiting (%s elapsed): %v", what, time.Since(start).Round(time.Second), last)
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	t.Fatalf("%s: timed out after %s: %v", what, timeout, last)
}

// dumpLogs tails a CloudWatch log group into the test output. It runs on
// failure, before teardown deletes the group along with everything else: a
// failed run that leaves nothing to read costs another twenty minutes and
// another cluster to reproduce.
//
// Shelling out to the CLI rather than adding the cloudwatchlogs SDK: `aws` is
// already a hard requirement for ECS Exec, and this is diagnostics, not
// assertions.
func (s *stack) dumpLogs(t *testing.T, group string, since time.Duration, maxLines int) {
	t.Helper()

	cmd := exec.Command("aws", "logs", "tail", group,
		"--region", s.region,
		"--since", fmt.Sprintf("%dm", int(since.Minutes())),
		"--format", "short")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("could not tail %s: %v: %s", group, err, truncate(collapse(string(out)), 300))
		return
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		t.Logf("--- %s: no events ---", group)
		return
	}
	dropped := 0
	if len(lines) > maxLines {
		dropped = len(lines) - maxLines
		lines = lines[dropped:]
	}
	t.Logf("--- %s: last %d lines (%d earlier lines dropped) ---", group, len(lines), dropped)
	for _, line := range lines {
		t.Logf("  %s", line)
	}
}

// --- ECS helpers -----------------------------------------------------------

func (s *stack) describeService(ctx context.Context, service string) (*ecstypes.Service, error) {
	out, err := s.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(s.cluster),
		Services: []string{service},
	})
	if err != nil {
		return nil, err
	}
	if len(out.Services) != 1 {
		return nil, fmt.Errorf("service %q not found", service)
	}
	return &out.Services[0], nil
}

// serviceSteady reports an error unless the service has exactly one deployment,
// it has completed, and every task it wants is running.
func (s *stack) serviceSteady(ctx context.Context, service string, want int) error {
	svc, err := s.describeService(ctx, service)
	if err != nil {
		return err
	}
	if int(svc.DesiredCount) != want {
		return fmt.Errorf("%s: desired count is %d, want %d", service, svc.DesiredCount, want)
	}
	if int(svc.RunningCount) != want || svc.PendingCount != 0 {
		return fmt.Errorf("%s: %d/%d running, %d pending", service, svc.RunningCount, want, svc.PendingCount)
	}
	if len(svc.Deployments) != 1 {
		return fmt.Errorf("%s: %d deployments in flight", service, len(svc.Deployments))
	}
	switch state := svc.Deployments[0].RolloutState; state {
	case ecstypes.DeploymentRolloutStateCompleted:
		return nil
	case ecstypes.DeploymentRolloutStateFailed:
		// The circuit breaker has already stopped retrying; so should we.
		return fmt.Errorf("%s: rollout %s: %s: %w", service, state,
			aws.ToString(svc.Deployments[0].RolloutStateReason), errTerminal)
	default:
		return fmt.Errorf("%s: rollout is %s: %s", service, state,
			aws.ToString(svc.Deployments[0].RolloutStateReason))
	}
}

func (s *stack) listTasks(ctx context.Context, service string) ([]ecstypes.Task, error) {
	listed, err := s.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:     aws.String(s.cluster),
		ServiceName: aws.String(service),
	})
	if err != nil {
		return nil, err
	}
	if len(listed.TaskArns) == 0 {
		return nil, nil
	}
	described, err := s.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(s.cluster),
		Tasks:   listed.TaskArns,
	})
	if err != nil {
		return nil, err
	}
	return described.Tasks, nil
}

// tasksHealthy checks the ECS health status, which for these task definitions
// is `muster -health-probe` against the control socket: it reports healthy only
// once every workload the script spawned is up and has passed readiness.
func (s *stack) tasksHealthy(ctx context.Context, service string, want int) error {
	tasks, err := s.listTasks(ctx, service)
	if err != nil {
		return err
	}
	running := 0
	for _, task := range tasks {
		if aws.ToString(task.LastStatus) != "RUNNING" {
			continue
		}
		running++
		if task.HealthStatus != ecstypes.HealthStatusHealthy {
			return fmt.Errorf("%s: task %s health is %s", service, taskID(task), task.HealthStatus)
		}
	}
	if running != want {
		return fmt.Errorf("%s: %d tasks running, want %d", service, running, want)
	}
	return nil
}

func taskID(task ecstypes.Task) string { return shortTaskID(aws.ToString(task.TaskArn)) }

// runningTasks returns a service's RUNNING tasks in a stable order, so the
// per-replica checks visit them predictably.
func (s *stack) runningTasks(ctx context.Context, service string) ([]ecstypes.Task, error) {
	tasks, err := s.listTasks(ctx, service)
	if err != nil {
		return nil, err
	}
	running := make([]ecstypes.Task, 0, len(tasks))
	for _, task := range tasks {
		if aws.ToString(task.LastStatus) == "RUNNING" {
			running = append(running, task)
		}
	}
	sort.Slice(running, func(i, j int) bool {
		return aws.ToString(running[i].TaskArn) < aws.ToString(running[j].TaskArn)
	})
	return running, nil
}

// pdGet queries whichever PD replica comes first. Use it for cluster-wide facts
// (any member serves them); query replicas individually when the point is to
// compare their answers.
func (s *stack) pdGet(ctx context.Context, path string, into any) error {
	tasks, err := s.runningTasks(ctx, s.pdService)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return errors.New("no PD task is running")
	}
	return s.pd.get(ctx, aws.ToString(tasks[0].TaskArn), path, into)
}

// taskIP returns the private address of a task's awsvpc ENI, which is also the
// address the muster script picked with ifaddr() and registered in CloudMap.
func taskIP(task ecstypes.Task) string {
	for _, att := range task.Attachments {
		for _, d := range att.Details {
			if aws.ToString(d.Name) == "privateIPv4Address" {
				return aws.ToString(d.Value)
			}
		}
	}
	return ""
}

// --- CloudMap helpers ------------------------------------------------------

func (s *stack) discover(ctx context.Context, service string) ([]sdtypes.HttpInstanceSummary, error) {
	out, err := s.sd.DiscoverInstances(ctx, &servicediscovery.DiscoverInstancesInput{
		NamespaceName: aws.String(s.namespace),
		ServiceName:   aws.String(service),
		HealthStatus:  sdtypes.HealthStatusFilterHealthy,
		MaxResults:    aws.Int32(100),
	})
	if err != nil {
		return nil, err
	}
	return out.Instances, nil
}

func instanceIPs(instances []sdtypes.HttpInstanceSummary) []string {
	ips := make([]string, 0, len(instances))
	for _, inst := range instances {
		if ip, ok := inst.Attributes["AWS_INSTANCE_IPV4"]; ok {
			ips = append(ips, ip)
		}
	}
	return ips
}
