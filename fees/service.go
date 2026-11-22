// Package fees is the Fees API: bills that accrue line items over a billing
// period and are totalled when closed.
package fees

import (
	"context"

	"encore.dev"
	"encore.dev/config"
	"github.com/reconquest/karma-go"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/lmeow01/pave-fees/fees/billflow"
)

// Config is loaded from config.cue.
type Config struct {
	TemporalHostPort  config.String
	TemporalNamespace config.String
}

var cfg = config.Load[*Config]()

// taskQueue is scoped by environment so that several environments can share
// one Temporal namespace without picking up each other's bills.
var taskQueue = encore.Meta().Environment.Name + "-fees"

// Service serves the Fees API and runs the Temporal worker for bill
// workflows in the same process.
//
//encore:service
type Service struct {
	temporal client.Client
	worker   worker.Worker
	store    store
}

func initService() (*Service, error) {
	c, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHostPort(),
		Namespace: cfg.TemporalNamespace(),
	})
	if err != nil {
		return nil, karma.Format(err, "dial temporal")
	}

	st := store{db: db}
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(billflow.Bill)
	w.RegisterActivity(&billflow.Activities{Store: st})
	if err := w.Start(); err != nil {
		c.Close()
		return nil, karma.Format(err, "start temporal worker")
	}

	return &Service{temporal: c, worker: w, store: st}, nil
}

// Shutdown lets running workflow tasks and activities finish before the
// Temporal connection is closed.
func (s *Service) Shutdown(context.Context) {
	s.worker.Stop()
	s.temporal.Close()
}
