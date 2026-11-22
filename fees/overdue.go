package fees

import (
	"context"
	"sync"
	"time"

	"encore.dev/cron"
	"encore.dev/rlog"
	"golang.org/x/sync/errgroup"

	"github.com/lmeow01/pave-fees/fees/billflow"
)

const (
	// overdueGrace leaves a bill's own period-end timer time to close it.
	overdueGrace = 5 * time.Minute
	// sweepTimeLimit keeps one sweep shorter than the interval between two.
	sweepTimeLimit = 8 * time.Minute
	sweepPageSize  = 100
	sweepParallel  = 10
)

var _ = cron.NewJob("close-overdue-bills", cron.JobConfig{
	Title:    "Close bills past their period end",
	Every:    10 * cron.Minute,
	Endpoint: CloseOverdueBills,
})

// CloseOverdueBillsResponse lists the bills a sweep closed and those it
// could not close.
type CloseOverdueBillsResponse struct {
	Closed []string `json:"closed"`
	Failed []string `json:"failed"`
}

// CloseOverdueBills closes open bills more than a few minutes past their
// period end. A bill whose workflow was never started, and that nobody wrote
// to since, is closed by nothing else. A bill that fails to close does not
// hold up the bills behind it.
//
//encore:api private method=POST path=/internal/bills/close-overdue
func (s *Service) CloseOverdueBills(ctx context.Context) (*CloseOverdueBillsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeLimit)
	defer cancel()

	var (
		mu   sync.Mutex
		resp = &CloseOverdueBillsResponse{Closed: []string{}, Failed: []string{}}
	)
	closeOne := func(params billflow.Params) {
		_, err := s.closeThroughWorkflow(ctx, params)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			rlog.Error("close overdue bill", "bill_id", params.BillID, "err", err)
			resp.Failed = append(resp.Failed, params.BillID)
			return
		}
		resp.Closed = append(resp.Closed, params.BillID)
	}

	cursor := overdueCursor{Before: time.Now().Add(-overdueGrace)}
	for ctx.Err() == nil {
		page, err := s.store.overdueBills(ctx, cursor, sweepPageSize)
		if err != nil {
			return nil, err
		}
		var g errgroup.Group
		g.SetLimit(sweepParallel)
		for _, params := range page {
			g.Go(func() error {
				closeOne(params)
				return nil
			})
		}
		_ = g.Wait()
		if len(page) < sweepPageSize {
			break
		}
		last := page[len(page)-1]
		cursor.AfterPeriodEnd, cursor.AfterID = last.PeriodEnd, last.BillID
	}
	return resp, nil
}
