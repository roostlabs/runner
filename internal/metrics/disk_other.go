//go:build !linux && !darwin

package metrics

func diskPercent(string) float64 { return 0 }
