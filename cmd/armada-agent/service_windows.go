package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "armada-agent"

func isService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

func defaultServiceLog() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Armada", "agent.log")
}

// agentService adapts the agent to the Service Control Manager.
type agentService struct {
	run func(context.Context) error
}

func (s *agentService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			// The agent stopped by itself: report a failure, so the
			// service's recovery actions start it again.
			if err != nil {
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		}
	}
}

func runService(run func(context.Context) error) error {
	return svc.Run(serviceName, &agentService{run: run})
}

// installService creates the service, or updates it if it exists: started
// at boot as LocalSystem, restarted when it fails or exits (self-updates and
// restart actions exit and rely on this).
func installService(configPath, logFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("config %s: %w", configPath, err)
	}
	args := []string{"-config", configPath}
	if logFile != "" {
		args = append(args, "-log-file", logFile)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	cfg := mgr.Config{
		DisplayName:      "Armada fleet agent",
		Description:      "Checks in with the Armada server, installs updates and reports health.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}
	s, err := m.OpenService(serviceName)
	if err == nil {
		// Update the existing service in place.
		cur, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		cur.DisplayName, cur.Description, cur.StartType, cur.DelayedAutoStart = cfg.DisplayName, cfg.Description, cfg.StartType, cfg.DelayedAutoStart
		cur.BinaryPathName = windows.EscapeArg(exe)
		for _, a := range args {
			cur.BinaryPathName += " " + windows.EscapeArg(a)
		}
		if err := s.UpdateConfig(cur); err != nil {
			s.Close()
			return err
		}
	} else {
		if s, err = m.CreateService(serviceName, exe, cfg, args...); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 10 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 24*60*60); err != nil {
		return fmt.Errorf("set restart on failure: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set restart on failure: %w", err)
	}
	fmt.Printf("service %s installed: %s %v\n", serviceName, exe, args)
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()
	if st, err := s.Control(svc.Stop); err == nil {
		for i := 0; i < 30 && st.State != svc.Stopped; i++ {
			time.Sleep(time.Second)
			if st, err = s.Query(); err != nil {
				break
			}
		}
	} else if !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Printf("service %s removed\n", serviceName)
	return nil
}
