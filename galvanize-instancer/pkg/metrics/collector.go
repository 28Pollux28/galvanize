package metrics

import (
	"context"

	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// DeploymentCollector
// ---------------------------------------------------------------------------

// DeploymentCollector implements prometheus.Collector and queries the database
// on each scrape to report current deployment counts by status, challenge, and team.
// This ensures metric accuracy even after restarts or manual DB changes.
type DeploymentCollector struct {
	db   *gorm.DB
	desc *prometheus.Desc
}

// NewDeploymentCollector creates a Collector backed by db.
// Call prometheus.MustRegister(collector) after creation.
func NewDeploymentCollector(db *gorm.DB) *DeploymentCollector {
	return &DeploymentCollector{
		db: db,
		desc: prometheus.NewDesc(
			"instancer_deployments",
			"Current number of deployments grouped by status, challenge, and team. "+
				"Note: 404 responses on /status, /extend, and /terminate are expected "+
				"when no deployment exists and do not indicate errors.",
			[]string{"status", "category", "challenge_name", "team_id"},
			nil,
		),
	}
}

// Describe sends the descriptor to the channel.
func (c *DeploymentCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect queries the database and sends deployment count metrics.
func (c *DeploymentCollector) Collect(ch chan<- prometheus.Metric) {
	type row struct {
		Status        string
		Category      string
		ChallengeName string
		TeamID        *string
		Count         int64
	}

	var rows []row
	// Include all non-deleted deployment statuses. Stopped records are deleted
	// immediately, but we include the status for completeness.
	c.db.Model(&models.Deployment{}).
		Select("status, category, challenge_name, team_id, COUNT(*) as count").
		Group("status, category, challenge_name, team_id").
		Scan(&rows)

	for _, r := range rows {
		teamID := ""
		if r.TeamID != nil {
			teamID = *r.TeamID
		}
		ch <- prometheus.MustNewConstMetric(
			c.desc,
			prometheus.GaugeValue,
			float64(r.Count),
			r.Status, r.Category, r.ChallengeName, teamID,
		)
	}
}

// ---------------------------------------------------------------------------
// QueueCollector
// ---------------------------------------------------------------------------

// QueueLengther is the minimal interface needed to observe Redis queue depth.
// It is satisfied by *worker.Queue without importing that package.
type QueueLengther interface {
	QueueLength(ctx context.Context) (int64, error)
}

// QueueCollector reports the current number of jobs waiting in the Redis queue.
type QueueCollector struct {
	queue QueueLengther
	desc  *prometheus.Desc
}

// NewQueueCollector creates a collector that reads queue depth from q on each scrape.
// Register it only when Redis is configured (queue != nil).
func NewQueueCollector(queue QueueLengther) *QueueCollector {
	return &QueueCollector{
		queue: queue,
		desc: prometheus.NewDesc(
			"instancer_queue_depth",
			"Number of jobs currently waiting in the Redis job queue",
			nil, nil,
		),
	}
}

// Describe sends the descriptor to the channel.
func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect queries the queue length and sends the gauge metric.
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	n, err := c.queue.QueueLength(context.Background())
	if err != nil {
		// Emit a stale-marker rather than silently dropping the metric.
		ch <- prometheus.NewInvalidMetric(c.desc, err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n))
}

