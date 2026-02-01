package pkg

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Define Metrics
var (
	activeInstances = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ctf_active_instances",
		Help: "The total number of currently running challenge containers",
	})
	deployOps = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ctf_deploy_ops_total",
		Help: "The total number of deployment operations attempted",
	})
	activeInstancesPerTeam = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ctf_active_instances_team",
			Help: "Active Instances per team",
		},
		[]string{"team_id"},
	)
	unauthorizedDeploymentsRequestsPerTeam = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ctf_unauthorized_deploy_requests_total",
			Help: "Total number of unauthorized deployment requests per team",
		},
		[]string{"team_id"},
	)
)
