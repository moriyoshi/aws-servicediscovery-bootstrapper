// Package tikv holds muster's TiKV-on-Fargate end-to-end test.
//
// The test provisions a real ECS cluster and asserts that muster brings a
// three-member PD Raft group and three TiKV stores up from nothing, so it is
// behind the e2e_tikv build tag and does nothing without MUSTER_E2E_TIKV=1.
// See README.md for what it costs and how to run it.
package tikv
