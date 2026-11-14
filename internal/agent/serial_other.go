//go:build !linux && !darwin && !windows

package agent

func platformSerial() string { return "" }
