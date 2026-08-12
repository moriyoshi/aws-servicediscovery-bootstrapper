data "aws_iam_policy_document" "ecr_repository" {
  statement {
    sid    = "PushPull"
    effect = "Allow"
    principals {
      type        = "AWS"
      identifiers = [local.root_user_arn]
    }
    actions = [
      "ecr:BatchGetImage",
      "ecr:BatchCheckLayerAvailability",
      "ecr:CompleteLayerUpload",
      "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
  }
  statement {
    sid    = "TaskExecutionPull"
    effect = "Allow"
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.ecs_task_execution.arn]
    }
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ]
  }
}

# force_delete lets `terraform destroy` tear the stack down without a manual
# pass to delete the pushed images first.
resource "aws_ecr_repository" "tikv_pd" {
  name                 = "${local.prefix}tikv-pd"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  tags = {
    Name = "${local.prefix}tikv-pd"
  }
}

resource "aws_ecr_repository_policy" "tikv_pd" {
  repository = aws_ecr_repository.tikv_pd.name
  policy     = data.aws_iam_policy_document.ecr_repository.json
}

resource "aws_ecr_repository" "tikv_node" {
  name                 = "${local.prefix}tikv-node"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  tags = {
    Name = "${local.prefix}tikv-node"
  }
}

resource "aws_ecr_repository_policy" "tikv_node" {
  repository = aws_ecr_repository.tikv_node.name
  policy     = data.aws_iam_policy_document.ecr_repository.json
}
