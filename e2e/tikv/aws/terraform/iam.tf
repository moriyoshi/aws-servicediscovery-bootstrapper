data "aws_iam_policy_document" "ecs_tasks_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# --- execution role (pulls images, writes logs) ----------------------------

resource "aws_iam_role" "ecs_task_execution" {
  name               = "${local.prefix}ecs-task-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume_role.json
  tags = {
    Name = "${local.prefix}ecs-task-execution"
  }
}

data "aws_iam_policy_document" "ecs_task_execution" {
  statement {
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = [
      "${aws_cloudwatch_log_group.tikv_pd.arn}:log-stream:*",
      "${aws_cloudwatch_log_group.tikv_node.arn}:log-stream:*",
    ]
  }
  statement {
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ]
    resources = [
      aws_ecr_repository.tikv_pd.arn,
      aws_ecr_repository.tikv_node.arn,
    ]
  }
}

resource "aws_iam_role_policy" "ecs_task_execution" {
  name   = "inline"
  role   = aws_iam_role.ecs_task_execution.name
  policy = data.aws_iam_policy_document.ecs_task_execution.json
}

# --- task roles (what the muster script itself is allowed to do) -----------

data "aws_iam_policy_document" "muster_common" {
  statement {
    sid    = "Discovery"
    effect = "Allow"
    actions = [
      "servicediscovery:DiscoverInstances",
      "servicediscovery:DiscoverInstancesRevision",
    ]
    # DiscoverInstances cannot be scoped to a namespace resource.
    resources = ["*"]
  }
  statement {
    sid    = "EcsPreconditions"
    effect = "Allow"
    actions = [
      "ecs:DescribeServices",
    ]
    resources = ["*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.main.arn]
    }
  }
  # ECS Exec is how the test driver queries PD: the cluster is not reachable
  # from outside the VPC by any other route.
  statement {
    sid    = "ExecuteCommand"
    effect = "Allow"
    actions = [
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role" "ecs_task_pd" {
  name               = "${local.prefix}ecs-task-pd"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume_role.json
  tags = {
    Name = "${local.prefix}ecs-task-pd"
  }
}

data "aws_iam_policy_document" "ecs_task_pd" {
  source_policy_documents = [data.aws_iam_policy_document.muster_common.json]

  statement {
    sid    = "SeedElection"
    effect = "Allow"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
    ]
    resources = [aws_dynamodb_table.kv.arn]
  }
}

resource "aws_iam_role_policy" "ecs_task_pd" {
  name   = "inline"
  role   = aws_iam_role.ecs_task_pd.name
  policy = data.aws_iam_policy_document.ecs_task_pd.json
}

resource "aws_iam_role" "ecs_task_tikv" {
  name               = "${local.prefix}ecs-task-tikv"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume_role.json
  tags = {
    Name = "${local.prefix}ecs-task-tikv"
  }
}

resource "aws_iam_role_policy" "ecs_task_tikv" {
  name   = "inline"
  role   = aws_iam_role.ecs_task_tikv.name
  policy = data.aws_iam_policy_document.muster_common.json
}
