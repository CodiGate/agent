package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/docker"
	"github.com/codigate/agent/importer"
	"github.com/codigate/agent/tunnel"
)

// discovery feeds Docker label rules into the ingress and, with create_apps, makes sure every
// hostname has an app behind this agent.
type discovery struct {
	ingress *tunnel.Ingress
	api     *importer.API // nil without create_apps
	logger  *slog.Logger

	mu      sync.Mutex
	agentID string
	rules   []config.Rule
	done    map[string]bool // hostnames that have an app (or can't get one)
	wake    chan struct{}
}

func startDiscovery(ctx context.Context, cfg *config.Config, ingress *tunnel.Ingress, logger *slog.Logger) (*discovery, error) {
	c, err := docker.NewClient(cfg.Docker.Endpoint)
	if err != nil {
		return nil, err
	}
	d := &discovery{ingress: ingress, logger: logger.With("component", "docker"), done: map[string]bool{}, wake: make(chan struct{}, 1)}
	if cfg.Docker.CreateApps {
		d.api = &importer.API{BaseURL: cfg.API, Token: cfg.APIToken, Client: &http.Client{Timeout: 30 * time.Second}}
		go d.createLoop(ctx)
	}
	go docker.Watch(ctx, c, cfg.Docker.Network, d.logger, d.apply)
	return d, nil
}

func (d *discovery) apply(rules []config.Rule) {
	d.ingress.SetDynamic(rules)
	d.mu.Lock()
	d.rules = rules
	d.mu.Unlock()
	d.poke()
}

// onSession learns the agent's id from the edge.
func (d *discovery) onSession(agentID, _ string) {
	d.mu.Lock()
	d.agentID = agentID
	d.mu.Unlock()
	d.poke()
}

func (d *discovery) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *discovery) createLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(5 * time.Minute): // retry transient failures
		}
		d.mu.Lock()
		agentID, rules := d.agentID, d.rules
		d.mu.Unlock()
		if agentID == "" {
			continue
		}
		for _, r := range rules {
			d.mu.Lock()
			skip := d.done[r.Hostname]
			d.mu.Unlock()
			if skip {
				continue
			}
			id, err := d.api.CreateApp(ctx, importer.AppName(r.Hostname), r.Hostname, agentID)
			var apiErr *importer.APIError
			switch {
			case err == nil:
				d.logger.Info("app created; set who may access it in the dashboard", "hostname", r.Hostname, "app", id)
			case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity:
				// In use (usually: the app exists already) or not under a verified domain.
				d.logger.Info("no app created", "hostname", r.Hostname, "reason", apiErr.Error())
			default:
				d.logger.Warn("creating app failed; will retry", "hostname", r.Hostname, "err", err)
				continue
			}
			d.mu.Lock()
			d.done[r.Hostname] = true
			d.mu.Unlock()
		}
	}
}
