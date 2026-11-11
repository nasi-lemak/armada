//go:build !linux && !windows

package agent

func processLister(name string) []string { return []string{"pgrep", "-x", name} }
