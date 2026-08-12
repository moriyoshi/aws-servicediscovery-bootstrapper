# Backs muster's kv_* builtins. PD uses a single leased key
# ("tikv-pd/seed") to elect exactly one bootstrapper on a cold start, so the
# schema is the one muster documents: `pk` as the partition key, `val` as the
# payload, `expires_at` as the TTL attribute.
resource "aws_dynamodb_table" "kv" {
  name         = "${local.prefix}kv"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }

  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  tags = {
    Name = "${local.prefix}kv"
  }
}
