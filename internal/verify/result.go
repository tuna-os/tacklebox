package verify

// CheckResult represents the outcome of a single verification assertion.
type CheckResult struct {
	Name string
	Err  error // nil = pass
}

func (r CheckResult) String() string {
	if r.Err == nil {
		return "  ✓ " + r.Name
	}
	return "  ✗ " + r.Name + ": " + r.Err.Error()
}

func (r CheckResult) Failed() bool {
	return r.Err != nil
}
