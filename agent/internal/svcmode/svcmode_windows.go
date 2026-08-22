//go:build windows

// Package svcmode wraps the agent's life as a real Windows Service:
// starts automatically at boot (before any user logs in), and can only be
// stopped by an administrator (net stop / services.msc / sc.exe) - not by
// the logged-in user, since service control requires elevated rights.
// Windows itself restarts it automatically after a crash via the recovery
// actions configured in Install.
package svcmode

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const ServiceName = "BackupAgent"

func IsWindowsService() bool {
	v, err := svc.IsWindowsService()
	return err == nil && v
}

type handler struct {
	run func(ctx context.Context)
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		h.run(ctx)
		close(done)
	}()

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
loop:
	for {
		select {
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				s <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				break loop
			}
		case <-done:
			break loop
		}
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	s <- svc.Status{State: svc.Stopped}
	return false, 0
}

// Run blocks for the lifetime of the service, invoking run(ctx) as the
// actual agent logic. ctx is cancelled when Windows asks the service to
// stop or the machine is shutting down.
func Run(run func(ctx context.Context)) error {
	return svc.Run(ServiceName, &handler{run: run})
}

// Install registers the agent as an auto-starting Windows Service running
// as LocalSystem, with recovery actions so it comes back after a crash,
// then starts it immediately. Must be run elevated (the installer runs
// with RequestExecutionLevel admin for exactly this reason).
func Install(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connexion au gestionnaire de services (droits administrateur requis ?): %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(ServiceName); err == nil {
		existing.Close()
		return fmt.Errorf("le service %s est déjà installé", ServiceName)
	}

	s, err := m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName: "Backup Agent",
		Description: "Sauvegarde automatique vers le serveur Backup Center.",
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return err
	}
	defer s.Close()

	// The mgr package has no recovery-action API; sc.exe (present on every
	// Windows install) is the standard way to set them.
	_ = exec.Command("sc.exe", "failure", ServiceName, "reset=", "86400",
		"actions=", "restart/5000/restart/5000/restart/60000").Run()

	return s.Start()
}

func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connexion au gestionnaire de services (droits administrateur requis ?): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service non installé: %w", err)
	}
	defer s.Close()

	_, _ = s.Control(svc.Stop)
	for i := 0; i < 20; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return s.Delete()
}
