//go:build race

package clusterstate

// raceEnabled disables only wall-clock performance assertions whose runtime is
// intentionally dominated by the race detector. Correctness and allocation
// budgets remain active in the same test.
const raceEnabled = true
