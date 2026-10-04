package admission

// Deterministic admits when average CPU usage is at or below allowance minus
// margin. It ignores Weight and is safe for concurrent use. Its zero value is
// ready to use and is the limiter's default policy.
type Deterministic struct{}

// CanProceed checks the threshold without drawing a random value.
func (Deterministic) CanProceed(s State) bool {
	s, valid := normalizeState(s)
	return valid && s.AverageCPUs <= s.AllowanceCPUs-s.MarginCPUs
}
