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
}

// ServerOpts holds the dependencies needed to construct a Server.
type ServerOpts struct {
	DB               *gorm.DB
	ChallengeIndexer challenge.ChallengeIndexer
	ConfigProvider   config.Provider
	Deployer         ansible.Deployer
	ExpiryScheduler  *scheduler.ExpiryScheduler
	KeyMutex         keymutex.KeyMutex
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

	s.wg.Add(1)
	_, err = models.CreateDeployment(s.db, chall.Name, claims.TeamID, chall.Category, conf.Instancer.DeploymentTTL, conf.Instancer.DeploymentMaxExtensions)
	if err != nil {
		_ = s.kmu.UnlockKey(id)
		s.wg.Done()
		zap.S().Errorf("Failed to create deployment record: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to create deployment record: %v", err))}) //TODO notify admin
	}
	_ = s.kmu.UnlockKey(id)

	go func() {
		defer s.wg.Done()
		deployOps.Inc()
		deployment, dbErr := models.GetDeployment(s.db, chall.Category, chall.Name, claims.TeamID, false)
		if dbErr != nil {
			zap.S().Errorf("Failed to get deployment for error update: %v", dbErr)
			return
		}

		connInfo, err := s.deployer.Deploy(context.Background(), conf, chall, claims.TeamID)
		if err != nil {
			zap.S().Errorf("Deploy failed for challenge %s team %s: %v", chall.Name, claims.TeamID, err)
			dbErr = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", err.Error())
			if dbErr != nil {
				zap.S().Errorf("Saving deployment error status failed: %v", dbErr)
			}
			return
		}

		err = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusRunning, connInfo, "")
		if err != nil {
			zap.S().Errorf("Failed to save deployment running status: %v", err)
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
	zap.S().Debugf("Status request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)

	chall, err := s.challIdx.Get(claims.Category, claims.ChallengeName)
	if err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid challenge")})
	}
	var deployment *models.Deployment
	if chall.Unique == true {
		deployment, err = models.GetUniqueDeployment(s.db, chall.Category, chall.Name, false)
	} else {
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
	s.wg.Add(1)
	tx := s.db.Begin()
	deployment, err := models.GetDeployment(tx, claims.Category, claims.ChallengeName, claims.TeamID, true)
	if err != nil {
		tx.Rollback()
		s.wg.Done()
		if errors.Is(err, models.ErrNotFound) {
			return ctx.JSON(404, api.Error{Message: utils.Ptr("No deployment found for this team and challenge")})
		}
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get deployment status: %v", err))})
	}
	if deployment.Status == models.DeploymentStatusStopping {
		tx.Rollback()
		s.wg.Done()
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Termination already in progress")})
	}
	err = models.UpdateDeploymentStatus(tx, deployment, models.DeploymentStatusStopping, "", "")
	if err != nil {
		tx.Rollback()
		s.wg.Done()
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to update deployment status: %v", err))})
	}
	tx.Commit()

	conf := s.confProv.GetConfig()
	go func() {
		defer s.wg.Done()
		err = models.TerminateDeployment(s.db, s.challIdx, s.deployer, conf, deployment)
		if err != nil {
			zap.S().Errorf("Failed to terminate instance: %v", err)
		}
	}()

	return ctx.NoContent(200)
}
