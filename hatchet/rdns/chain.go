package rdns

// ShouldChain says whether a successful run of resolve-ips starts the next one at once:
// pick-networks filled the whole run (more /24s wait) and trace stored at least one trace
// (so the /24s picked are not picked again, and the chain makes progress). The caller
// checks that no task failed (it runs this after the last task succeeded) and that the
// run is a Hatchet one (the run-rdns command never chains).
func ShouldChain(pick PickOutput, trace TraceOutput) bool {
	return pick.Limit > 0 && pick.Picked == pick.Limit && trace.Stored > 0
}
