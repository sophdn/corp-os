package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b // BUG: should be a + b
}
