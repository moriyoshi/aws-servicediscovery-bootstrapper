# AWS ServiceDiscovery (a.k.a. Cloud Map) bootstrapper

AWS ServiceDiscovery bootstrapper is a helper utility that enables any executables to run with arguments interpolated with instance attributes associated in CloudMap services.

For example, if you have a CloudMap namespace `my-namespace` and a service `my-service` with two instances `192.168.0.2` and `192.168.0.3`, each assigned a port attribute of `8000`, running the following command will invoke `my-executable` with the argument `--servers=http://192.168.0.2:8000,http://192.168.0.3:8000`

```bash
aws-service-discovery-bootstrapper \
  -namespace my-namespace \
  <executable> \
    '--servers={{ instances "my-service" | extract "IPv4Addr,Port" | mapprintf "http://%s:%d" | join "," }}'
```

## Installation

You can install the AWS ServiceDiscovery bootstrapper using `go get`:

```bash
go install github.com/moriyoshi/aws-service-discovery-bootstrapper@latest
```

## Usage

```bash
aws-service-discovery-bootstrapper \
  -namespace <namespace> \
  -health-status <health-status> \
  -retry <retry-count> \
  -execution-delay-jitter <delay> \
  -execution-delay-jitter-unit <unit> \
  [-no-fail] \
  [-respawn [-respawn-keep-alive] [-respawn-max-retries <n>] ...] \
  [-healthcheck-type <http|https|tcp|grpc> -healthcheck-target <target> ...] \
  [-control-socket <path>] \
  -- <executable> [args...]
```

<dl>
<dt><code>-namespace</code></dt>
<dd>

**Required.** The namespace to use for service discovery.
</dd>
<dt><code>-health-status</code></dt>
<dd>

The health status to use for service discovery. Default is `HEALTHY`.

Valid values are:
- `HEALTHY`: Include only healthy instances.
- `UNHEALTHY`: Include only unhealthy instances.
- `ALL`: Include all instances.
- `HEALTHY_OR_ELSE_ALL`: Include only healthy instances, or all instances if no healthy instances are found.
</dd>
<dt><code>-retry</code></dt>
<dd>
The number of times to retry service discovery if no instances that matches the specified health status are found. Default is `3`.
</dd>
<dt><code>-precondition</code></dt>
<dd>

A precondition to check before running the command. If the precondition is not met, the command will not be executed.

Valid values are:
- `AllEcsTasksRunning`: The command will only be executed if all ECS tasks in the cluster are running.
</dd>
<dt><code>-precondition-check-timeout</code></dt>
<dd>

The timeout for the precondition check. If the precondition check does not complete within the specified timeout, the command will not be executed. The timeout can be specified with a suffix of `s` (seconds), `ms` (milliseconds), `us` (microseconds), or `ns` (nanoseconds). Default is `30s`.
</dd>
<dt><code>-execution-delay-jitter</code></dt>
<dd>

The amount of jitter that delays the command execution. This is useful to give more chance to the command to run successfully if the services being discovered are not available yet.  The amount can be specified with a suffix of `s` (seconds), `ms` (milliseconds), `us` (microseconds), or `ns` (nanoseconds). Default is `0s`.
</dd>
<dt><code>-execution-delay-jitter-unit</code></dt>
<dd>

The unit of the execution delay jitter. Some of valid values are `1s` (seconds), `1ms` (milliseconds), `1us` (microseconds), `1ns` (nanoseconds). Default is `1s`.
</dd>
<dt><code>-no-fail</code></dt>
<dd>

If specified, `instances` function will not fail if no instances that match the specified health status are found. Note that retries will still be attempted if this option is specified.
</dd>
<dt><code>-respawn</code></dt>
<dd>

Restart the workload when it exits with a **non-zero** status, up to `-respawn-max-retries` times, with exponential backoff between restarts. A clean exit (status `0`) ends the harness successfully unless `-respawn-keep-alive` is also given. This lets the container survive transient workload failures. Default is off. See [Respawning](#respawning).
</dd>
<dt><code>-respawn-keep-alive</code></dt>
<dd>

Also restart the workload when it exits cleanly (status `0`), i.e. keep it alive regardless of exit status. Implies `-respawn` semantics for exit code `0`. Default is off.
</dd>
<dt><code>-respawn-max-retries</code></dt>
<dd>

Maximum number of **consecutive** restarts before the harness gives up and exits non-zero. `0` means unlimited. The counter is reset once the workload has stayed up for at least `-respawn-reset-after` (see below). Default is `5`.
</dd>
<dt><code>-respawn-initial-interval</code> / <code>-respawn-max-interval</code> / <code>-respawn-multiplier</code></dt>
<dd>

The exponential-backoff parameters used between restarts: the first interval, the ceiling, and the multiplier. Defaults are `1s`, `60s`, and `2.0`. Backoff is jittered.
</dd>
<dt><code>-respawn-reset-after</code></dt>
<dd>

If the workload stays up at least this long, the retry counter and backoff are reset (min-healthy time). This prevents a workload that runs fine for a while and then crashes from eventually exhausting its retries. Default is `30s`.
</dd>
<dt><code>-shutdown-grace</code></dt>
<dd>

Grace period between `SIGTERM` and `SIGKILL` when terminating the workload (on harness shutdown or a health-triggered restart). When the harness receives `SIGTERM`/`SIGINT`, it relays `SIGTERM` to the workload and waits up to this long before force-killing. Default is `10s`.
</dd>
<dt><code>-healthcheck-type</code></dt>
<dd>

Enable a background workload healthcheck of the given type: `http`, `https`, `tcp`, or `grpc`. Empty (the default) disables healthchecking. See [Healthcheck](#healthcheck).
</dd>
<dt><code>-healthcheck-target</code></dt>
<dd>

The probe target. For `http`/`https` it is a URL (a `2xx` response is healthy). For `tcp` it is `host:port` (a successful connection is healthy). For `grpc` it is `host:port` of a server implementing the standard [gRPC Health Checking Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md) (a `SERVING` status is healthy). gRPC is probed over plaintext HTTP/2 (h2c).
</dd>
<dt><code>-healthcheck-interval</code> / <code>-healthcheck-timeout</code></dt>
<dd>

Interval between probes and the per-probe timeout. The timeout must be positive and less than the interval. Defaults are `10s` and `2s`.
</dd>
<dt><code>-healthcheck-healthy-threshold</code> / <code>-healthcheck-unhealthy-threshold</code></dt>
<dd>

The number of consecutive successful / failed probes required to transition to `healthy` / `unhealthy`. Defaults are `1` and `3`.
</dd>
<dt><code>-healthcheck-start-period</code></dt>
<dd>

A grace period before the first probe during which probe failures are ignored (they do not count toward the unhealthy threshold), giving the workload time to start. Default is `0`.
</dd>
<dt><code>-healthcheck-action</code></dt>
<dd>

What to do when the workload becomes `unhealthy`: `none` (default, observational — the state is only exposed via the control socket) or `restart` (kill and respawn the workload; requires `-healthcheck-type` and is subject to the respawn retry/backoff settings).
</dd>
<dt><code>-healthcheck-grpc-service</code></dt>
<dd>

The service name passed in the gRPC `HealthCheckRequest`. Empty (the default) queries overall server health.
</dd>
<dt><code>-control-socket</code></dt>
<dd>

Path to a unix-domain socket on which the harness serves `GET /health` (JSON). Exposes harness-level state (uptime, respawn count, current backoff) and the workload health. Empty (the default) disables it. See [Control socket and health probe](#control-socket-and-health-probe).
</dd>
<dt><code>-health-probe</code></dt>
<dd>

Run the binary as a **health-probe client** instead of the harness: it connects to `-control-socket`, and exits `0` when healthy or non-zero otherwise. Intended for use as a container `HEALTHCHECK CMD`. See [Control socket and health probe](#control-socket-and-health-probe).
</dd>
<dt><code>&lt;executable&gt;</code></dt>
<dd>

**Required.** The executable to run with the interpolated arguments.
</dd>
<dt><code>[args...]</code></dt>
<dd>

The arguments to pass to the executable. These can include interpolated values from CloudMap services. How the interpolation works is described below.
</dl>

### `env` emulation

If the first token of the command is literally `env`, the bootstrapper emulates the `env` utility: any leading `NAME=VALUE` operands are consumed and set as environment variables on the executable that follows. The first operand that is not of the form `NAME=VALUE` begins the actual command.

```bash
aws-service-discovery-bootstrapper \
  -namespace <namespace> \
  -- env SERVERS='{{ join "," (extract "IPv4Addr" (instances "my-service")) }}' <executable> [args...]
```

The `VALUE` part is interpolated with the same functions available for arguments (see [Interpolation](#interpolation) below), so computed values can be passed as environment variables. Because the variables are handed directly to the process rather than through a shell, discovered values are safe even if they contain shell metacharacters.

Only the `NAME=VALUE` form is supported. `env` options (such as `-i`, `-u`, `-C`, or `-S`) are **not** supported; an operand starting with `-` is treated as the command to run and will fail as "command not found". Assignments are applied in order and override any inherited variable of the same name.

## Interpolation

The AWS ServiceDiscovery bootstrapper uses the [go-template](https://golang.org/pkg/text/template/) syntax for interpolation. The following functions are available:

- `instances <service-name>`: Returns a slice of structs that describes instances for the specified service name.
    Each struct contains the following fields:
    - `IPv4Addr`: The IPv4 address of the instance.
    - `IPv6Addr`: The IPv6 address of the instance.
    - `Port`: The port of the instance.

- `extract <attribute> <slice>`: For each item of a slice, extracts the specified attribute(s) from the instances, and returns the slice of slices of extracted attributes. `<attribute>` can be a comma-separated list of attributes (e.g. `IPv4Addr,Port`).

    Example: `extract "IPv4Addr,Port"` will return a slice of slices, where each inner slice contains the IPv4 address and port of an instance.

- `exclude <ip-addr> <slice>`: Excludes a item whose any of IP addresses corresponds to the specified IP address from the slice. The IP address can be an IPv4 or IPv6 address.

    Example: `exclude (ifaddr "192.168.0.0/24")` will exclude the instance whose IPv4 address matches the host's IP address.

- `mapprintf <format> <input>`: For each item of a slice, formats the value with the specified format string. The format is done using the [fmt.Sprintf](https://golang.org/pkg/fmt/#Sprintf) syntax.
   
   Example: `mapprintf "http://%s:%d"` will format the IPv4 address and port of each instance into a URL.

- `join <separator> <input>`: Joins the items of a slice into a single string, separated by the specified separator.

    Example: `join ","` will join the items of a slice with a comma.

- `ifaddr <CIDR>`: Returns the address that matches the CIDR if exists.

    Example: `ifaddr 192.168.0.0/24` will return the IPv4 address of the instance that matches the CIDR.

## Respawning

By default the bootstrapper runs the workload once and exits with its status. With `-respawn`, it instead supervises the workload and restarts it on failure, so the container can survive transient failures:

- A **non-zero** exit triggers a restart (up to `-respawn-max-retries`, with exponential backoff). Add `-respawn-keep-alive` to also restart on a clean exit.
- The retry counter and backoff reset once the workload has been up for at least `-respawn-reset-after`, so a long-lived workload that eventually crashes is not penalised by earlier restarts.
- When the retries are exhausted, the harness exits non-zero.
- The harness is termination-signal aware: on `SIGTERM`/`SIGINT` it relays `SIGTERM` to the workload, waits up to `-shutdown-grace`, then `SIGKILL`s it. Such a shutdown is not counted as a failure and does not trigger a respawn.

```bash
aws-service-discovery-bootstrapper \
  -namespace <namespace> \
  -respawn -respawn-max-retries 10 -respawn-initial-interval 1s -respawn-max-interval 60s \
  -- <executable> [args...]
```

## Healthcheck

With `-healthcheck-type`, a background goroutine probes the workload on an interval and tracks a tri-state health (`unknown` → `healthy` / `unhealthy`) using consecutive-success/failure thresholds and an optional start period:

- `http` / `https`: an HTTP `GET` of `-healthcheck-target`; a `2xx` response is healthy.
- `tcp`: a TCP connection to `host:port`; a successful connect is healthy.
- `grpc`: the standard [gRPC Health Checking Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md) `Check` over plaintext HTTP/2 (h2c); a `SERVING` status is healthy. Use `-healthcheck-grpc-service` to query a specific service.

The healthcheck is **observational** by default — the state is exposed via the control socket. Set `-healthcheck-action=restart` to have a sustained-unhealthy workload killed and respawned (subject to the respawn settings above).

## Control socket and health probe

With `-control-socket <path>`, the harness serves `GET /health` (JSON) over a unix-domain socket, reporting both harness-level state and workload health:

```json
{
  "harness":  { "running": true, "started_at": "…", "uptime_seconds": 123.4 },
  "workload": { "up": true, "pid": 4321, "started_at": "…", "uptime_seconds": 100.2,
                "respawn_count": 2, "current_backoff_seconds": 0, "max_retries": 5,
                "last_exit_code": 0, "last_exit_error": "" },
  "health":   { "state": "healthy", "consecutive_ok": 3, "consecutive_fail": 0,
                "last_probe_at": "…", "last_probe_error": "" }
}
```

The same binary can act as a probe client with `-health-probe`, which queries the socket and maps state to an exit code (`0` = healthy, non-zero = unhealthy or unreachable). This makes it usable directly as a container healthcheck command:

```dockerfile
ENTRYPOINT ["aws-service-discovery-bootstrapper", "-namespace", "my-namespace", \
            "-respawn", \
            "-healthcheck-type", "http", "-healthcheck-target", "http://127.0.0.1:8080/healthz", \
            "-control-socket", "/run/harness.sock", \
            "--", "my-workload"]
HEALTHCHECK CMD ["aws-service-discovery-bootstrapper", "-health-probe", "-control-socket", "/run/harness.sock"]
```

The probe client uses a short timeout, so a missing socket or dead harness reports unhealthy promptly rather than hanging.

## Configuration

The AWS ServiceDiscovery bootstrapper can be configured using environment variables supported by AWS SDK. The following is a non-exhaustive list of such:

- `AWS_REGION`: The AWS region to use for service discovery. If not set, the default region from the AWS CLI configuration will be used.

- `AWS_PROFILE`: The AWS profile to use for service discovery. If not set, the default profile from the AWS CLI configuration will be used.

- `AWS_ACCESS_KEY_ID`: The AWS access key ID to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SECRET_ACCESS_KEY`: The AWS secret access key to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SESSION_TOKEN`: The AWS session token to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_ENDPOINT_URL`: The AWS endpoint URL to use for service discovery. If not set, the default endpoint URL from the AWS CLI configuration will be used. Specifying this will effectively disable the endpoint prefixing behavior. (Thus the actual endpoint will end up being the same as the endpoint URL, in contrast to `data-servicediscovery.<region>.amazonaws.com` where the endpoint is `servicediscovery.<region>.amazonaws.com`.)
