-- The overdue-bill sweep pages through open bills by period end.
CREATE INDEX open_bills_by_period_end ON bills (period_end, id) WHERE status = 'open';
