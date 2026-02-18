package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// DeployDurationSeconds tracks how long Ansible deploy operations take.
	// Labels allow aggregation globally, per challenge, and per team.
	DeployDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instancer_deploy_duration_seconds",
			Help:    "Duration of challenge deploy operations in seconds",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600},
		},
		[]string{"category", "challenge_name", "team_id"},
	)

	// TerminateDurationSeconds tracks how long Ansible terminate operations take.
	TerminateDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instancer_terminate_duration_seconds",
			Help:    "Duration of challenge terminate operations in seconds",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600},
		},
		[]string{"category", "challenge_name", "team_id"},
	)

	// DeployOpsTotal counts completed deploy operations by challenge and outcome.
	// result label is "success" or "error".
	DeployOpsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_deploy_ops_total",
			Help: "Total number of completed deploy operations by challenge and result",
		},
		[]string{"category", "challenge_name", "result"},
	)

	// TerminateOpsTotal counts completed terminate operations by challenge and outcome.
	// result label is "success" or "error".
	TerminateOpsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_terminate_ops_total",
			Help: "Total number of completed terminate operations by challenge and result",
		},
		[]string{"category", "challenge_name", "result"},
	)

	// UnauthorizedDeployRequestsTotal counts requests where a team attempted to
	// deploy or terminate a challenge they are not authorized for.
	UnauthorizedDeployRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_unauthorized_deploy_requests_total",
			Help: "Total number of unauthorized deployment requests per team",
		},
		[]string{"team_id"},
	)

	// ExtendOpsTotal counts successful deployment extension operations.
	ExtendOpsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_extend_ops_total",
			Help: "Total number of successful deployment extension operations",
		},
		[]string{"category", "challenge_name"},
	)

	// ExtendRejectedTotal counts rejected extension requests by reason.
	// reason: window_not_reached | no_extensions_left | already_expired | unknown
	ExtendRejectedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_extend_rejected_total",
			Help: "Total number of rejected deployment extension requests by reason",
		},
		[]string{"category", "challenge_name", "reason"},
	)

	// DeployConflictTotal counts 409 conflicts where a team attempted to deploy
	// a challenge that already has an active deployment.
	DeployConflictTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_deploy_conflict_total",
			Help: "Total number of deploy conflicts (challenge already deployed)",
		},
		[]string{"category", "challenge_name"},
	)

	// JobRetriesTotal counts worker job retries due to transient errors.
	JobRetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_job_retries_total",
			Help: "Total number of worker job retries due to transient errors",
		},
		[]string{"category", "challenge_name", "job_type"},
	)

	// JobPermanentFailuresTotal counts jobs that failed permanently (exhausted retries or non-transient error).
	JobPermanentFailuresTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instancer_job_permanent_failures_total",
			Help: "Total number of worker jobs that failed permanently",
		},
		[]string{"category", "challenge_name", "job_type"},
	)

	// DeploymentLifetimeSeconds tracks the total time a deployment was alive,
	// from creation to successful termination. Buckets are in seconds.
	DeploymentLifetimeSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instancer_deployment_lifetime_seconds",
			Help:    "Total lifetime of a deployment from creation to termination",
			Buckets: []float64{5 * 60, 15 * 60, 30 * 60, 60 * 60, 2 * 3600, 4 * 3600, 8 * 3600, 24 * 3600},
		},
		[]string{"category", "challenge_name"},
	)

	// JobQueueWaitSeconds tracks how long jobs wait in the Redis queue before
	// being picked up by a worker. Only meaningful when Redis is configured.
	JobQueueWaitSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instancer_job_queue_wait_seconds",
			Help:    "Time jobs spend waiting in the Redis queue before processing",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300},
		},
		[]string{"job_type"},
	)

	// ChallengesIndexed reports how many challenges are currently indexed per category.
	// Reset and re-set on every BuildIndex call so removed challenges disappear.
	ChallengesIndexed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "instancer_challenges_indexed",
			Help: "Number of challenges currently indexed per category",
		},
		[]string{"category"},
	)
)

// SetChallengesIndexed resets and repopulates the ChallengesIndexed gauge from
// a category → count map. Call this after every BuildIndex to keep it current.
func SetChallengesIndexed(categoryCounts map[string]int) {
	ChallengesIndexed.Reset()
	for cat, count := range categoryCounts {
		ChallengesIndexed.WithLabelValues(cat).Set(float64(count))
	}
}

