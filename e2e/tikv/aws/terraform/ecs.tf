resource "aws_cloudwatch_log_group" "tikv_pd" {
  name              = "${local.prefix}tikv-pd"
  retention_in_days = var.log_retention_days
  tags = {
    Name = "${local.prefix}tikv-pd"
  }
}

resource "aws_cloudwatch_log_group" "tikv_node" {
  name              = "${local.prefix}tikv-node"
  retention_in_days = var.log_retention_days
  tags = {
    Name = "${local.prefix}tikv-node"
  }
}

resource "aws_ecs_cluster" "main" {
  name = "${local.prefix}main"

  setting {
    name  = "containerInsights"
    value = "disabled"
  }

  tags = {
    Name = "${local.prefix}main"
  }
}

locals {
  # Replace one task at a time, never two.
  #
  # ECS defaults to minimumHealthyPercent 100 / maximumPercent 200, which for a
  # three-task service means it may start three replacements and stop three
  # originals at once. For the store tier that is total data loss -- the volumes
  # are ephemeral, so every replacement is a new, empty store -- and for PD it
  # is the loss of Raft quorum.
  #
  # Sized so the ceiling is desired + 1: ECS allows floor(desired * pct / 100)
  # tasks, so 4/3 rounded up gives 134% for three tasks and 125% for four. With
  # minimumHealthyPercent at 100 the sequence is forced to be start one, wait
  # for it to be healthy, stop one, repeat -- and "healthy" now means PD has
  # accepted the store into the cluster, not merely that a port answers.
  pd_one_at_a_time_pct   = ceil((var.pd_desired_count + 1) / var.pd_desired_count * 100)
  tikv_one_at_a_time_pct = ceil((var.tikv_desired_count + 1) / var.tikv_desired_count * 100)

  control_socket = "/tmp/muster.sock"

  # `muster -health-probe` reports the container healthy only once every
  # workload spawned by the script is up and its readiness probe has passed, so
  # the ECS health status doubles as an assertion on the script's own view.
  #
  # Both tiers coordinate before their workload starts — PD elects a
  # bootstrapper, TiKV waits for PD — and report unhealthy while they do, so
  # they need a grace period. There are two, and only one of them is big enough:
  #
  #   startPeriod (here)                 capped at 300s; RegisterTaskDefinition
  #                                      rejects anything larger outright
  #   healthCheckGracePeriodSeconds      set on the service, uncapped, and since
  #     (see the services below)         ECS extended it beyond load balancers
  #                                      it covers container health checks too
  #
  # So take the full 300s here, and let the service-level grace period carry the
  # rest of a slow cold start.
  muster_health_check = {
    command     = ["CMD", "/muster", "-health-probe", "-control-socket", local.control_socket]
    interval    = 15
    timeout     = 5
    retries     = 4
    startPeriod = 300 # maximum ECS accepts
  }

  # Generous: it only delays the scheduler noticing a task that is never going
  # to come up, and the deployment circuit breaker still fails the apply.
  health_check_grace_period = 900

  # MUSTER_PD_SERVICE is a CloudMap discovery name, for instances().
  # MUSTER_SELF_GROUP / MUSTER_SELF_SERVICE are this replica set's own
  # coordinates -- the ECS cluster and service -- for all_replicas_running().
  #
  # The latter two are otherwise read from the task metadata endpoint. They are
  # passed explicitly because the fallback is a single best-effort lookup at
  # startup with no retry, and when it comes up empty the builtin can only
  # raise — which aborts resolve() on every attempt and leaves the workload
  # permanently unstarted. Terraform knows both names for certain, so it says
  # so rather than depending on that path. (Reading the metadata endpoint is
  # also how this stack found the bug where muster looked for the wrong
  # environment variable and so never had metadata on ECS at all.)
  #
  # MUSTER_SUBNET_CIDR is the same idea one layer down: the scripts advertise
  # SELF.ipv4, which also comes from that metadata endpoint, and fall back to
  # picking the address off the interface with ifaddr(). It is optional to the
  # scripts and passed here so the fallback is actually available.
  muster_env = [
    { name = "MUSTER_SUBNET_CIDR", value = var.internal_subnet_cidr },
    { name = "MUSTER_PD_SERVICE", value = local.pd_discovery_name },
    { name = "MUSTER_PD_REPLICAS", value = tostring(var.pd_desired_count) },
    # tikv.star compares the stores PD believes in against the stores that
    # exist; pd.star will not act on a discovery answer that does not account
    # for a majority of this many.
    { name = "MUSTER_TIKV_SERVICE", value = local.tikv_discovery_name },
    { name = "MUSTER_TIKV_REPLICAS", value = tostring(var.tikv_desired_count) },
    { name = "MUSTER_SELF_GROUP", value = aws_ecs_cluster.main.name },
  ]
}

# --- PD --------------------------------------------------------------------

resource "aws_ecs_task_definition" "tikv_pd" {
  family                   = "${local.prefix}tikv-pd"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.pd_cpu
  memory                   = var.pd_memory
  execution_role_arn       = aws_iam_role.ecs_task_execution.arn
  task_role_arn            = aws_iam_role.ecs_task_pd.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  tags = {
    Name = "${local.prefix}tikv-pd"
  }

  container_definitions = jsonencode([
    {
      name      = "default"
      essential = true
      image     = "${aws_ecr_repository.tikv_pd.repository_url}:${var.image_tag}"

      # muster resolves argv, elects the bootstrapper and supervises pd-server;
      # everything after `--` is the script's COMMAND global.
      entryPoint = [
        "/muster",
        "-namespace", aws_service_discovery_http_namespace.tikv.name,
        "-script", "/etc/muster/pd.star",
        "-kv-store", aws_dynamodb_table.kv.name,
        "-control-socket", local.control_socket,
        "--",
      ]
      command = ["/pd-server"]

      environment = concat(local.muster_env, [
        { name = "MUSTER_SELF_SERVICE", value = local.pd_service_name },
      ])

      portMappings = [
        { name = "client", protocol = "tcp", containerPort = 2379, hostPort = 2379 },
        { name = "peer", protocol = "tcp", containerPort = 2380, hostPort = 2380 },
      ]

      healthCheck = local.muster_health_check

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.tikv_pd.name
          awslogs-stream-prefix = "ecs"
          awslogs-region        = local.aws_region
          mode                  = "non-blocking"
        }
      }

      linuxParameters = { initProcessEnabled = true }
    }
  ])
}

resource "aws_ecs_service" "tikv_pd" {
  name            = local.pd_service_name
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.tikv_pd.arn
  desired_count   = var.pd_desired_count
  launch_type     = "FARGATE"

  enable_execute_command            = true
  wait_for_steady_state             = var.wait_for_steady_state
  health_check_grace_period_seconds = local.health_check_grace_period

  # See locals: one at a time.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = local.pd_one_at_a_time_pct

  network_configuration {
    subnets          = [aws_subnet.internal.id]
    security_groups  = [aws_security_group.tasks.id]
    assign_public_ip = false
  }

  service_connect_configuration {
    enabled   = true
    namespace = aws_service_discovery_http_namespace.tikv.arn
    service {
      port_name      = "client"
      discovery_name = local.pd_discovery_name
      client_alias {
        port     = 2379
        dns_name = "tikv-pd"
      }
    }
  }

  # Surface a broken deployment as an apply failure instead of retrying
  # forever; rollback is pointless here since there is no previous revision.
  deployment_circuit_breaker {
    enable   = true
    rollback = false
  }

  tags = {
    Name = "${local.prefix}tikv-pd"
  }

  depends_on = [
    aws_iam_role_policy.ecs_task_execution,
    aws_iam_role_policy.ecs_task_pd,
    aws_vpc_endpoint.interface,
    aws_vpc_endpoint.s3,
    aws_vpc_endpoint.dynamodb,
  ]
}

# --- TiKV ------------------------------------------------------------------

resource "aws_ecs_task_definition" "tikv_node" {
  family                   = "${local.prefix}tikv-node"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.tikv_cpu
  memory                   = var.tikv_memory
  execution_role_arn       = aws_iam_role.ecs_task_execution.arn
  task_role_arn            = aws_iam_role.ecs_task_tikv.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  volume {
    name = "data"
  }

  tags = {
    Name = "${local.prefix}tikv-node"
  }

  container_definitions = jsonencode([
    {
      name      = "default"
      essential = true
      image     = "${aws_ecr_repository.tikv_node.repository_url}:${var.image_tag}"

      entryPoint = [
        "/muster",
        "-namespace", aws_service_discovery_http_namespace.tikv.name,
        "-script", "/etc/muster/tikv.star",
        "-control-socket", local.control_socket,
        "--",
      ]
      command = ["/tikv-server"]

      environment = concat(local.muster_env, [
        { name = "MUSTER_SELF_SERVICE", value = local.tikv_service_name },
      ])

      mountPoints = [
        { sourceVolume = "data", containerPath = "/db", readOnly = false },
      ]

      ulimits = [
        { name = "nofile", softLimit = 123880, hardLimit = 123880 },
      ]

      portMappings = [
        { name = "service", protocol = "tcp", containerPort = 20160, hostPort = 20160 },
        { name = "status", protocol = "tcp", containerPort = 20180, hostPort = 20180 },
      ]

      healthCheck = local.muster_health_check

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.tikv_node.name
          awslogs-stream-prefix = "ecs"
          awslogs-region        = local.aws_region
          mode                  = "non-blocking"
        }
      }

      linuxParameters = { initProcessEnabled = true }
    }
  ])
}

resource "aws_ecs_service" "tikv_node" {
  name            = local.tikv_service_name
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.tikv_node.arn
  desired_count   = var.tikv_desired_count
  launch_type     = "FARGATE"

  enable_execute_command            = true
  wait_for_steady_state             = var.wait_for_steady_state
  health_check_grace_period_seconds = local.health_check_grace_period

  # See locals: one at a time.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = local.tikv_one_at_a_time_pct

  network_configuration {
    subnets          = [aws_subnet.internal.id]
    security_groups  = [aws_security_group.tasks.id]
    assign_public_ip = false
  }

  service_connect_configuration {
    enabled   = true
    namespace = aws_service_discovery_http_namespace.tikv.arn
    service {
      port_name      = "service"
      discovery_name = local.tikv_discovery_name
      client_alias {
        port     = 20160
        dns_name = "tikv-node"
      }
    }
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = false
  }

  tags = {
    Name = "${local.prefix}tikv-node"
  }

  depends_on = [
    aws_iam_role_policy.ecs_task_execution,
    aws_iam_role_policy.ecs_task_tikv,
    aws_vpc_endpoint.interface,
    aws_vpc_endpoint.s3,

    # Not just for tidiness: a store blocks in resolve() until PD is
    # discoverable and answering, and PD's own cold start has been observed to
    # take upwards of eight minutes. The PD service waits for steady state, so
    # ordering behind it turns that into a wait of seconds and keeps the stores
    # off the retry path entirely. It also tears the stores down before PD.
    aws_ecs_service.tikv_pd,
  ]
}
