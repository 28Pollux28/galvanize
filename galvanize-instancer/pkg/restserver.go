package pkg

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/scheduler"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/28Pollux28/galvanize/pkg/worker"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"k8s.io/utils/keymutex"
)

// Server implements api.ServerInterface
type Server struct {
	db          *gorm.DB
	challIdx    challenge.ChallengeIndexer
	confProv    config.Provider
	deployer    ansible.Deployer
	expirySched *scheduler.ExpiryScheduler
	kmu         keymutex.KeyMutex
	wg          sync.WaitGroup
	jobQueue    *worker.Queue // Redis job queue (optional, nil means use direct goroutines)
}

// ServerOpts holds the dependencies needed to construct a Server.
type ServerOpts struct {
	DB               *gorm.DB
	ChallengeIndexer challenge.ChallengeIndexer
	ConfigProvider   config.Provider
	Deployer         ansible.Deployer
	ExpiryScheduler  *scheduler.ExpiryScheduler
	KeyMutex         keymutex.KeyMutex
	JobQueue         *worker.Queue // Optional: if provided, jobs are queued to Redis
}

var _ api.ServerInterface = (*Server)(nil)

// NewServerWithOpts creates a Server from explicitly provided dependencies.
// Mandatory dependencies are DB, ChallengeIndexer, and ConfigProvider.
// Deployer will default to AnsibleDeployer and KeyMutex will default to a hashed key mutex if not provided.
func NewServerWithOpts(opts ServerOpts) *Server {
	kmu := opts.KeyMutex
	if kmu == nil {
		kmu = keymutex.NewHashed(20)
	}
	deployer := opts.Deployer
	if deployer == nil {
		deployer = &ansible.AnsibleDeployer{}
	}
	return &Server{
		db:          opts.DB,
		challIdx:    opts.ChallengeIndexer,
		confProv:    opts.ConfigProvider,
		deployer:    deployer,
		expirySched: opts.ExpiryScheduler,
		kmu:         kmu,
		jobQueue:    opts.JobQueue,
	}
}

// StartScheduler launches the expiry scheduler in a background goroutine.
// The caller is responsible for cancelling ctx when shutdown begins.
func (s *Server) StartScheduler(ctx context.Context, sched *scheduler.ExpiryScheduler) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		sched.Start(ctx)
	}()
}

// Wait blocks until all background goroutines have completed.
func (s *Server) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

//// SyncMetrics queries Docker to find actual running state and updates Prometheus
//func (s *Server) SyncMetrics() {
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//
//	containers, err := s.DockerCli.ContainerList(ctx, client.ContainerListOptions{All: true})
//	if err != nil {
//		log.Printf("Failed to sync metrics: %v", err)
//		return
//	}
//
//	count := 0.0
//	for _, c := range containers.Items {
//		// Our naming convention is /chall_{challId}_{userId}
//		// Docker names return with a leading slash usually
//		for _, name := range c.Names {
//			if strings.Contains(name, "chall_") && c.State == "running" {
//				count++
//				break
//			}
//			//TODO remove
//			count++
//			activeInstancesPerTeam.WithLabelValues(name).Set(count)
//		}
//	}
//	activeInstances.Set(count)
//	log.Printf("Metrics synced. Found %.0f active challenge containers.", count)
//}

func (s *Server) GetHealth(ctx echo.Context) error {
	return ctx.JSON(200, map[string]string{"status": "ok"})
}

func (s *Server) DeployInstance(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	var req api.DeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	zap.S().Infof("Deploy request received for challenge %s for team %s", req.ChallengeName, claims.TeamID)
	// Check if challenge is valid for team
	if req.ChallengeName != claims.ChallengeName && claims.Role != "admin" {
		zap.S().Errorf("Unauthorized attempt to deploy challenge %s for team %s", req.ChallengeName, claims.TeamID)
		unauthorizedDeploymentsRequestsPerTeam.WithLabelValues(claims.TeamID).Inc()
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	chall, err := s.challIdx.Get(req.Category, req.ChallengeName)
	if err != nil {
		zap.S().Errorf("Failed to get challenge infos: %v", err)
		return ctx.JSON(400, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get challenge info: %v", err))}) //TODO notify admin
	}
	if chall.Unique == true {
		zap.S().Errorf("Attempt to deploy unique challenge %s for team %s", req.ChallengeName, claims.TeamID)
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	id := chall.Name + claims.TeamID
	s.kmu.LockKey(id)
	existingDeployment, err := models.GetDeployment(s.db, chall.Category, chall.Name, claims.TeamID, false)
	if err == nil && existingDeployment != nil {
		_ = s.kmu.UnlockKey(id)
		return ctx.JSON(409, api.Error{Message: utils.Ptr("Challenge already deployed for this team")})
	}
	if err != nil && !errors.Is(err, models.ErrNotFound) {
		_ = s.kmu.UnlockKey(id)
		zap.S().Errorf("Failed to check existing deployments: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to check existing deployments: %v", err))}) //TODO notify admin
	}
	conf := s.confProv.GetConfig()

	deployment, err := models.CreateDeployment(s.db, chall.Name, claims.TeamID, chall.Category, conf.Instancer.DeploymentTTL, conf.Instancer.DeploymentMaxExtensions)
	if err != nil {
		_ = s.kmu.UnlockKey(id)
		zap.S().Errorf("Failed to create deployment record: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to create deployment record: %v", err))}) //TODO notify admin
	}
	_ = s.kmu.UnlockKey(id)

	// If we have a job queue, enqueue the deploy job
	if s.jobQueue != nil {
		job := worker.NewDeployJob(chall.Category, chall.Name, claims.TeamID, deployment.ID)
		if err := s.jobQueue.Enqueue(ctx.Request().Context(), job); err != nil {
			zap.S().Errorf("Failed to enqueue deploy job: %v", err)
			_ = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "Failed to queue deployment")
			return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug("Failed to queue deployment")})
		}
		zap.S().Infof("Deploy job queued for challenge %s team %s", chall.Name, claims.TeamID)
		deployOps.Inc()
		return ctx.NoContent(202)
	}

	// Fallback to direct goroutine if no queue configured
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		deployOps.Inc()

		connInfo, deployErr := s.deployer.Deploy(context.Background(), conf, chall, claims.TeamID)
		if deployErr != nil {
			zap.S().Errorf("Deploy failed for challenge %s team %s: %v", chall.Name, claims.TeamID, deployErr)
			dbErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", deployErr.Error())
			if dbErr != nil {
				zap.S().Errorf("Saving deployment error status failed: %v", dbErr)
			}
			return
		}

		updateErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusRunning, connInfo, "")
		if updateErr != nil {
			zap.S().Errorf("Failed to save deployment running status: %v", updateErr)
			return
		}
		zap.S().Infof("Deployment of challenge %s for team %s completed successfully.", chall.Name, claims.TeamID)
		activeInstancesPerTeam.WithLabelValues(claims.TeamID).Inc()
	}()

	return ctx.NoContent(202)
}

func (s *Server) GetInstanceStatus(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	chall, err := s.challIdx.Get(claims.Category, claims.ChallengeName)
	if err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid challenge")})
	}
	var deployment *models.Deployment
	if chall.Unique == true {
		zap.S().Debugf("Status request received for challenge %s", chall.Name)
		deployment, err = models.GetUniqueDeployment(s.db, chall.Category, chall.Name, false)
	} else {
		zap.S().Debugf("Status request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)
		deployment, err = models.GetDeployment(s.db, chall.Category, chall.Name, claims.TeamID, false)
	}
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			if chall.Unique && claims.Role != "admin" {
				return ctx.JSON(404, api.Error{Message: utils.Ptr("cannot deploy unique challenge")})
			}
			return ctx.NoContent(404)
		}
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get deployment status: %v", err))})
	}

	switch deployment.Status {
	case models.DeploymentStatusStarting:
		return ctx.JSON(200, api.StatusResponse{
			Status: utils.Ptr(api.StatusResponseStatusStarting),
		})
	case models.DeploymentStatusRunning:
		response := api.StatusResponse{
			Status:         utils.Ptr(api.StatusResponseStatusRunning),
			ConnectionInfo: &deployment.ConnectionInfo,
			Unique:         &chall.Unique,
		}
		// Only include expiration info for non-unique deployments
		if !chall.Unique {
			conf := s.confProv.GetConfig()
			response.ExpirationTime = deployment.ExpiresAt
			response.ExtensionsLeft = &deployment.TimeExtensionLeft
			response.ExtensionTime = utils.Ptr(utils.FormatDuration(conf.Instancer.DeploymentTTLExtension))
		}
		return ctx.JSON(200, response)
	case models.DeploymentStatusError:
		return ctx.JSON(500, api.Error{
			Message: utils.Ptr("An error occurred during deployment. Admins have been notified."),
		})
	case models.DeploymentStatusStopping:
		return ctx.JSON(200, api.StatusResponse{
			Status: utils.Ptr(api.StatusResponseStatusStopping),
		})
	}
	// TODO notify admin ?
	return ctx.JSON(500, api.Error{
		Message: utils.Ptr("Unknown deployment status"),
	})
}

func (s *Server) ExtendInstance(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	zap.S().Debugf("Extend request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)

	d, err := models.GetDeployment(s.db, claims.Category, claims.ChallengeName, claims.TeamID, false)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			return ctx.NoContent(404)
		}
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get deployment status: %v", err))})
	}

	conf := s.confProv.GetConfig()
	err = models.ExtendDeploymentExpiration(s.db, d, conf.Instancer.DeploymentTTLExtension, conf.Instancer.DeploymentExtensionWindow, conf.Instancer.DeploymentMaxExtensions)
	if err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr(err.Error())})
	}

	s.expirySched.NotifyChange(d.ID)

	return ctx.JSON(200, api.StatusResponse{
		Status:         utils.Ptr(api.StatusResponseStatusRunning),
		ConnectionInfo: &d.ConnectionInfo,
		ExpirationTime: d.ExpiresAt,
		ExtensionsLeft: &d.TimeExtensionLeft,
		ExtensionTime:  utils.Ptr(utils.FormatDuration(conf.Instancer.DeploymentTTLExtension)),
	})
}

func (s *Server) TerminateInstance(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	zap.S().Debugf("Terminate request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)

	var req api.DeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	// Check if challenge is valid for team
	if req.ChallengeName != claims.ChallengeName && claims.Role != "admin" {
		zap.S().Errorf("Unauthorized attempt to terminate challenge %s for team %s", req.ChallengeName, claims.TeamID)
		unauthorizedDeploymentsRequestsPerTeam.WithLabelValues(claims.TeamID).Inc()
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	tx := s.db.Begin()
	deployment, err := models.GetDeployment(tx, claims.Category, claims.ChallengeName, claims.TeamID, true)
	if err != nil {
		tx.Rollback()
		if errors.Is(err, models.ErrNotFound) {
			return ctx.JSON(404, api.Error{Message: utils.Ptr("No deployment found for this team and challenge")})
		}
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get deployment status: %v", err))})
	}
	if deployment.Status == models.DeploymentStatusStopping {
		tx.Rollback()
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Termination already in progress")})
	}
	err = models.UpdateDeploymentStatus(tx, deployment, models.DeploymentStatusStopping, "", "")
	if err != nil {
		tx.Rollback()
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to update deployment status: %v", err))})
	}
	tx.Commit()

	conf := s.confProv.GetConfig()

	// If we have a job queue, enqueue the terminate job
	if s.jobQueue != nil {
		chall, challErr := s.challIdx.Get(claims.Category, claims.ChallengeName)
		if challErr != nil {
			zap.S().Errorf("Failed to get challenge for terminate: %v", challErr)
			return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug("Failed to get challenge info")})
		}
		job := worker.NewTerminateJob(chall.Category, chall.Name, claims.TeamID, deployment.ID)
		if err := s.jobQueue.Enqueue(ctx.Request().Context(), job); err != nil {
			zap.S().Errorf("Failed to enqueue terminate job: %v", err)
			_ = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "Failed to queue termination")
			return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug("Failed to queue termination")})
		}
		zap.S().Infof("Terminate job queued for challenge %s team %s", claims.ChallengeName, claims.TeamID)
		return ctx.NoContent(200)
	}

	// Fallback to direct goroutine if no queue configured
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		terminateErr := models.TerminateDeployment(s.db, s.challIdx, s.deployer, conf, deployment)
		if terminateErr != nil {
			zap.S().Errorf("Failed to terminate instance: %v", terminateErr)
		}
	}()

	return ctx.NoContent(200)
}
