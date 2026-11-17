//go:build race

package main

// raceOn: the tests run with the race detector, which makes every request
// slower and allocate more, so TestPageBudgets checks only sizes then.
const raceOn = true
