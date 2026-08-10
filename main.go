package main

import (
	"bytes"
	"context"
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
	"reflect"
	"slices"
	"strconv"
	"strings"
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
)

var namespace string
var healthStatus string
var precondition string
var preconditionCheckTimeout time.Duration
var retryCount int
var noFail bool
var executionDelayJitter time.Duration
var executionDelayJitterUnit time.Duration

func init() {
	flag.StringVar(&namespace, "namespace", "", "The namespace of the instance to be listed")
	flag.StringVar(&healthStatus, "health-status", "HEALTHY", "The health status of the instance to be listed")
	flag.StringVar(&precondition, "precondition", "AllEcsTasksRunning", "Precondition that needs to be met before running the command. Supported values: AllEcsTasksRunning")
	flag.DurationVar(&preconditionCheckTimeout, "precondition-check-timeout", 30*time.Second, "The timeout for the precondition check")
	flag.IntVar(&retryCount, "retry-count", 10, "The number of times to retry the request")
	flag.BoolVar(&noFail, "no-fail", false, "Do not fail if no instances are found")
	flag.DurationVar(&executionDelayJitter, "execution-delay-jitter", 0, "The amount of jitter that delays the command execution.")
	flag.DurationVar(&executionDelayJitterUnit, "execution-delay-jitter-unit", time.Second, "The unit of the jitter that delays the command execution")
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

func doIt(ctx context.Context, logger *slog.Logger) error {
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if retryCount < 0 {
		return fmt.Errorf("retry count must be greater than or equal to 0")
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
		preconditionFunc(ctx, &cfg, 3*time.Second, preconditionCheckTimeout)
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
	cmd := exec.CommandContext(ctx, renderedCmdLine[0], renderedCmdLine[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run command: %w", err)
	}
	return nil
}

func main() {
	flag.Parse()
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := doIt(ctx, logger); err != nil {
		logger.Error("failed", slog.Any("err", err))
		os.Exit(1)
	}
}
