//go:build !windows

package main

import (
	"context"
	"errors"
)

func isService() bool { return false }

func runService(func(context.Context) error) error { return errors.New("not a Windows service") }

func defaultServiceLog() string { return "" }

var errNotWindows = errors.New("-install-service and -uninstall-service are for Windows; on Linux and macOS use install.sh")

func installService(string, string) error { return errNotWindows }

func uninstallService() error { return errNotWindows }
