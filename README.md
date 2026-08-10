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

## Configuration

The AWS ServiceDiscovery bootstrapper can be configured using environment variables supported by AWS SDK. The following is a non-exhaustive list of such:

- `AWS_REGION`: The AWS region to use for service discovery. If not set, the default region from the AWS CLI configuration will be used.

- `AWS_PROFILE`: The AWS profile to use for service discovery. If not set, the default profile from the AWS CLI configuration will be used.

- `AWS_ACCESS_KEY_ID`: The AWS access key ID to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SECRET_ACCESS_KEY`: The AWS secret access key to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SESSION_TOKEN`: The AWS session token to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_ENDPOINT_URL`: The AWS endpoint URL to use for service discovery. If not set, the default endpoint URL from the AWS CLI configuration will be used. Specifying this will effectively disable the endpoint prefixing behavior. (Thus the actual endpoint will end up being the same as the endpoint URL, in contrast to `data-servicediscovery.<region>.amazonaws.com` where the endpoint is `servicediscovery.<region>.amazonaws.com`.)
