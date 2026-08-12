package aws

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/moriyoshi/muster/internal/provider"
)

// DynamoKV is the DynamoDB-backed provider.KVStore. It relies on conditional writes for
// atomicity and on an application-side expiry filter (DynamoDB TTL deletion is
// lazy, so items can linger past expires_at). Election reads use ConsistentRead.
//
// Item schema: pk (S, partition key), val (S), expires_at (N, Unix epoch seconds,
// the table's TTL attribute), owner (S, the writer's identity).
type DynamoKV struct {
	client *dynamodb.Client
	table  string
	prefix string
	owner  string
}

var (
	_ provider.KVStore     = (*DynamoKV)(nil)
	_ provider.Provisioner = (*DynamoKV)(nil)
)

func NewDynamoKV(client *dynamodb.Client, table, prefix, owner string) *DynamoKV {
	return &DynamoKV{client: client, table: table, prefix: prefix, owner: owner}
}

func (d *DynamoKV) key(k string) string {
	if d.prefix == "" {
		return k
	}
	return d.prefix + "/" + k
}

func nowUnix() int64 { return time.Now().Unix() }

func numAV(v int64) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func strAV(v string) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

// isConditionalCheckFailed reports whether err is DynamoDB's
// ConditionalCheckFailedException, which we treat as a non-error negative result
// (someone else holds the lock / the value didn't match) rather than a failure.
func isConditionalCheckFailed(err error) bool {
	var e *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &e)
}

func (d *DynamoKV) item(key, val string, ttl time.Duration) map[string]ddbtypes.AttributeValue {
	item := map[string]ddbtypes.AttributeValue{
		"pk":    strAV(d.key(key)),
		"val":   strAV(val),
		"owner": strAV(d.owner),
	}
	if ttl > 0 {
		item["expires_at"] = numAV(nowUnix() + int64(ttl.Seconds()))
	}
	return item
}

func (d *DynamoKV) PutIfAbsent(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &d.table,
		Item:                d.item(key, val, ttl),
		ConditionExpression: aws.String("attribute_not_exists(pk) OR expires_at < :now"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": numAV(nowUnix()),
		},
	})
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kv put_if_absent %q: %w", key, err)
	}
	return true, nil
}

func (d *DynamoKV) CompareAndSwap(ctx context.Context, key, oldV, newV string, ttl time.Duration) (bool, error) {
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &d.table,
		Item:                d.item(key, newV, ttl),
		ConditionExpression: aws.String("val = :old AND (attribute_not_exists(expires_at) OR expires_at > :now)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":old": strAV(oldV),
			":now": numAV(nowUnix()),
		},
	})
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kv compare_and_swap %q: %w", key, err)
	}
	return true, nil
}

func (d *DynamoKV) Get(ctx context.Context, key string) (string, bool, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &d.table,
		ConsistentRead: aws.Bool(true),
		Key:            map[string]ddbtypes.AttributeValue{"pk": strAV(d.key(key))},
	})
	if err != nil {
		return "", false, fmt.Errorf("kv get %q: %w", key, err)
	}
	if out.Item == nil {
		return "", false, nil
	}
	if exp, ok := out.Item["expires_at"].(*ddbtypes.AttributeValueMemberN); ok {
		if n, perr := strconv.ParseInt(exp.Value, 10, 64); perr == nil && n <= nowUnix() {
			return "", false, nil // expired but not yet reaped by DynamoDB TTL
		}
	}
	v, ok := out.Item["val"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", false, nil
	}
	return v.Value, true, nil
}

func (d *DynamoKV) Delete(ctx context.Context, key string, ifValue *string) (bool, error) {
	input := &dynamodb.DeleteItemInput{
		TableName: &d.table,
		Key:       map[string]ddbtypes.AttributeValue{"pk": strAV(d.key(key))},
	}
	if ifValue != nil {
		input.ConditionExpression = aws.String("val = :old")
		input.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{":old": strAV(*ifValue)}
	}
	_, err := d.client.DeleteItem(ctx, input)
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kv delete %q: %w", key, err)
	}
	return true, nil
}

func (d *DynamoKV) Renew(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("kv renew %q: ttl must be positive", key)
	}
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           &d.table,
		Key:                 map[string]ddbtypes.AttributeValue{"pk": strAV(d.key(key))},
		UpdateExpression:    aws.String("SET expires_at = :new"),
		ConditionExpression: aws.String("attribute_exists(pk) AND expires_at > :now AND #o = :self"),
		ExpressionAttributeNames: map[string]string{
			"#o": "owner",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":new":  numAV(nowUnix() + int64(ttl.Seconds())),
			":now":  numAV(nowUnix()),
			":self": strAV(d.owner),
		},
	})
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kv renew %q: %w", key, err)
	}
	return true, nil
}

// Provision creates the KV table (on-demand billing) and enables TTL on the
// expires_at attribute if the table does not already exist. Used only when
// -kv-create-table is set; normally the table is provisioned out of band.
func (d *DynamoKV) Provision(ctx context.Context) error {
	_, err := d.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &d.table})
	if err == nil {
		return nil // already exists
	}
	var notFound *ddbtypes.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		return fmt.Errorf("describe kv table: %w", err)
	}
	_, err = d.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &d.table,
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		return fmt.Errorf("create kv table: %w", err)
	}
	waiter := dynamodb.NewTableExistsWaiter(d.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: &d.table}, 2*time.Minute); err != nil {
		return fmt.Errorf("waiting for kv table: %w", err)
	}
	_, err = d.client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: &d.table,
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("expires_at"),
		},
	})
	if err != nil {
		return fmt.Errorf("enable kv table ttl: %w", err)
	}
	return nil
}
