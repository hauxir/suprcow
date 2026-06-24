package manager

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/hauxir/suprcow/internal/config"
)

// reloadClient pings reload endpoints. The dev server recompiles synchronously
// before responding, so the timeout must allow for a full recompile.
var reloadClient = &http.Client{Timeout: 120 * time.Second}

// defaultReload GETs url to trigger a request-driven recompile. Any HTTP
// response means the reloader ran; the status code is irrelevant.
func defaultReload(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := reloadClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// triggerReloads pings each configured reload endpoint so a request-driven dev
// server recompiles the just-updated source. Best effort: failures are logged.
func (m *Manager) triggerReloads(ctx context.Context, pr int) {
	for _, t := range m.cfg.ReloadTrigger {
		url := fmt.Sprintf("http://%s:%d%s", serviceAlias(m.project, pr, t.Service), t.Port, t.Path)
		if err := m.reload(ctx, url); err != nil {
			log.Printf("reload trigger pr=%d service=%s: %v", pr, t.Service, err)
		}
	}
}

// defaultHealthTimeout bounds readiness waiting when a health check omits one.
const defaultHealthTimeout = 120 * time.Second

// waitHealthy blocks until every configured health gate for the PR passes, or
// returns an error if any gate times out. Services are reached on the shared
// network by their stable alias.
func (m *Manager) waitHealthy(ctx context.Context, pr int) error {
	for svc, hc := range m.cfg.Health {
		alias := serviceAlias(m.project, pr, svc)
		if err := m.ready(ctx, alias, hc); err != nil {
			return fmt.Errorf("service %s: %w", svc, err)
		}
	}
	return nil
}

// defaultReady polls a service's health gate (HTTP 2xx/3xx or TCP connect)
// until it passes or the gate's timeout elapses.
func (m *Manager) defaultReady(ctx context.Context, alias string, hc config.HealthCheck) error {
	timeout := hc.Timeout.Duration()
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		if hc.HTTP != "" {
			lastErr = probeHTTP(ctx, alias, hc.HTTP)
		} else {
			lastErr = probeTCP(ctx, alias, hc.TCP)
		}
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s: %v", timeout, lastErr)
		case <-ticker.C:
		}
	}
}

func probeHTTP(ctx context.Context, alias, path string) error {
	url := "http://" + alias + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func probeTCP(ctx context.Context, alias string, port int) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(alias, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}
