package api

import "github.com/prometheus/client_golang/prometheus"

// Metrics for canonical state_diff path & replay-vs-canonical divergence detection.
//
// Registered at package init time. Names are stable; alerting/dashboards can be wired
// against them after Phase 7 deployment.
var (
	canonicalStateDiffDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "trace_debankblock_canonical_duration_seconds",
		Help:    "Wall time spent building the canonical state_diff via Erigon reads.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms .. ~16s
	})

	canonicalReplayDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "trace_debankblock_replay_duration_seconds",
		Help:    "Wall time spent on the replay best-effort path (traces / error events).",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})

	replayDivergedBlocksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "trace_debankblock_replay_diverged_blocks_total",
		Help: "Number of blocks where at least one tx had replay receipt status != canonical receipt status.",
	})

	replayDivergedTxsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "trace_debankblock_replay_diverged_tx_total",
		Help: "Number of individual tx receipts where replay status differs from canonical history.",
	})

	replayFailedBlocksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "trace_debankblock_replay_failed_blocks_total",
		Help: "Number of blocks where replay returned an error or panicked (canonical state_diff still served).",
	})

	canonicalHistoryUnavailableTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "trace_debankblock_canonical_history_unavailable_total",
		Help: "Number of trace_debankBlock requests rejected because the requested height is below archive history availability.",
	})

	canonicalCodeMissingTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "trace_debankblock_canonical_code_missing_total",
		Help: "Number of canonical state_diff builds that found a non-empty codeHash with no entry in kv.Code (writer commit gap).",
	})
)

func init() {
	prometheus.MustRegister(
		canonicalStateDiffDuration,
		canonicalReplayDuration,
		replayDivergedBlocksTotal,
		replayDivergedTxsTotal,
		replayFailedBlocksTotal,
		canonicalHistoryUnavailableTotal,
		canonicalCodeMissingTotal,
	)
}
