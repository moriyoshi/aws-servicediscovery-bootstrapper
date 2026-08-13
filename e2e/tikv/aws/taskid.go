package aws

import "strings"

// shortTaskID reduces an ECS task ARN to the id at its tail, for log lines and
// error messages where the full ARN is noise.
//
// Outside the e2e_tikv build tag so it stays covered by the ordinary test run.
func shortTaskID(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}
