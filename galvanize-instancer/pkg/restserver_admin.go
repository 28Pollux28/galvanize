package pkg

import (
	"context"
	"errors"
	"fmt"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/utils"
	results "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (s *Server) ConfigCheck(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		zap.S().Debugf("Failed to get claims: %v", err)
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden")})
	}
	return ctx.NoContent(200)
}

func (s *Server) ListUniqueChallenges(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		zap.S().Debugf("Failed to get claims: %v", err)
	}
	zap.S().Debugf("Admin request for challenge list")
	if claims.Role != "admin" {
		zap.S().Debugf("Forbidden - Admin access required")
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}
	challenges := s.challIdx.GetAllUnique()
	challengesResp := make([]api.ChallengeCategoryResponse, 0)
	for _, chall := range challenges {
		challengesResp = append(challengesResp, api.ChallengeCategoryResponse{
			Category:      chall.Category,
			ChallengeName: chall.Name,
		})
	}

	return ctx.JSON(200, challengesResp)
}

func (s *Server) DeployAdminInstance(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}

	var req api.AdminDeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	zap.S().Infof("Admin deploy request received for challenge %s", req.ChallengeName)

	chall, err := s.challIdx.Get(req.Category, req.ChallengeName)
	if err != nil {
		zap.S().Errorf("Failed to get challenge infos: %v", err)
		return ctx.JSON(400, api.Error{Message: utils.Ptr(fmt.Sprintf("Failed to get challenge info: %v", err))})
	}
	if chall.Unique != true {
		zap.S().Errorf("Attempt to admin-deploy non-unique challenge %s", req.ChallengeName)
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Challenge is not unique")})
	}

	id := chall.Name + "_unique"
	s.kmu.LockKey(id)
	existingDeployment, err := models.GetUniqueDeployment(s.db, chall.Category, chall.Name, false)
	if err == nil && existingDeployment != nil {
		_ = s.kmu.UnlockKey(id)
		return ctx.JSON(409, api.Error{Message: utils.Ptr("Challenge already deployed")})
	}
	if err != nil && !errors.Is(err, models.ErrNotFound) {
		_ = s.kmu.UnlockKey(id)
		zap.S().Errorf("Failed to check existing deployments: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to check existing deployments: %v", err))})
	}

	s.wg.Add(1)
	_, err = models.CreateUniqueDeployment(s.db, chall.Name, chall.Category)
	if err != nil {
		_ = s.kmu.UnlockKey(id)
		s.wg.Done()
		zap.S().Errorf("Failed to create deployment record: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to create deployment record: %v", err))})
	}
	_ = s.kmu.UnlockKey(id)

	conf := config.Get()
	executor, resultsBuff := ansible.PreparePlaybook(conf, "create", chall, "", chall.DeployParameters)
	go func() {
		defer s.wg.Done()
		deployOps.Inc()
		deployment, dbErr := models.GetUniqueDeployment(s.db, chall.Category, chall.Name, false)
		if dbErr != nil {
			zap.S().Errorf("Failed to get deployment for update: %v", dbErr)
			return
		}
		if err := executor.Execute(context.Background()); err != nil {
			zap.S().Errorf("Ansible deploy failed: %v", err)

			dbErr = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", err.Error())
			if dbErr != nil {
				zap.S().Errorf("Saving deployment error status failed: %v", dbErr)
				return
			}
			res, err := results.ParseJSONResultsStream(resultsBuff)
			if err != nil {
				zap.S().Errorf("Failed to parse Ansible results: %v", err)
			}
			zap.S().Errorf("Ansible deploy fail reason: %s", res.String())
			return
		}

		containerInfos, err := ansible.ExtractContainerInfo(resultsBuff)
		if err != nil {
			zap.S().Errorf("Failed to extract container info: %v", err)
		}

		if len(containerInfos) == 0 {
			zap.S().Errorf("No container info found in Ansible results for challenge %s", req.ChallengeName)
			err := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "No container info found in Ansible results")
			if err != nil {
				zap.S().Errorf("Failed to save deployment error status: %v", err)
			}
			return
		}
		connInfo, err := ansible.GetConnectionInfo(containerInfos, conf.Instancer.InstancerHost)
		if err != nil {
			zap.S().Errorf("Failed to build connection info: %v", err)
			dbErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "Failed to build connection info: "+err.Error())
			if dbErr != nil {
				zap.S().Errorf("Failed to save deployment error status: %v", err)
			}
			return
		}

		err = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusRunning, connInfo, "")
		if err != nil {
			zap.S().Errorf("Failed to save deployment running status: %v", err)
			return
		}
		zap.S().Infof("Admin deployment of challenge %s completed successfully.", chall.Name)
	}()

	return ctx.NoContent(202)
}

func (s *Server) TerminateAdminInstance(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}

	var req api.AdminDeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	zap.S().Infof("Admin terminate request received for challenge %s", req.ChallengeName)

	s.wg.Add(1)
	tx := s.db.Begin()
	deployment, err := models.GetUniqueDeployment(tx, req.Category, req.ChallengeName, true)
	if err != nil {
		tx.Rollback()
		s.wg.Done()
		if errors.Is(err, models.ErrNotFound) {
			return ctx.JSON(404, api.Error{Message: utils.Ptr("No deployment found for this challenge")})
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

	conf := config.Get()
	go func() {
		defer s.wg.Done()
		err = models.TerminateDeployment(s.db, s.challIdx, conf, deployment)
		if err != nil {
			zap.S().Errorf("Failed to terminate instance: %v", err)
		}
	}()

	return ctx.NoContent(200)
}

func (s *Server) DeployAllAdminInstances(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}

	// Get all unique challenges from the index
	uniqueChallenges := s.challIdx.GetAllUnique()
	if len(uniqueChallenges) == 0 {
		return ctx.JSON(200, api.BulkOperationResponse{
			Message:         "No unique challenges found",
			ChallengesCount: 0,
		})
	}

	zap.S().Infof("Admin deploy-all request received for %d unique challenges", len(uniqueChallenges))
	deployed := 0

	conf := config.Get()
	challenges := make([]api.ChallengeCategoryResponse, 0)

	for _, chall := range uniqueChallenges {
		// Check if already deployed
		existingDeployment, err := models.GetUniqueDeployment(s.db, chall.Category, chall.Name, false)
		if err == nil && existingDeployment != nil {
			zap.S().Debugf("Challenge %s already deployed, skipping", chall.Name)
			continue
		}

		// Create deployment record
		s.wg.Add(1)
		_, err = models.CreateUniqueDeployment(s.db, chall.Name, chall.Category)
		if err != nil {
			s.wg.Done()
			zap.S().Errorf("Failed to create deployment record for %s: %v", chall.Name, err)
			continue
		}

		deployed++
		challenges = append(challenges, api.ChallengeCategoryResponse{})
		executor, resultsBuff := ansible.PreparePlaybook(conf, "create", chall, "", chall.DeployParameters)

		// Deploy in goroutine
		go func(ch *challenge.Challenge) {
			defer s.wg.Done()
			deployOps.Inc()
			deployment, dbErr := models.GetUniqueDeployment(s.db, ch.Category, ch.Name, false)
			if dbErr != nil {
				zap.S().Errorf("Failed to get deployment for %s: %v", ch.Name, dbErr)
				return
			}
			if err := executor.Execute(context.Background()); err != nil {
				zap.S().Errorf("Ansible deploy failed for %s: %v", ch.Name, err)
				dbErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", err.Error())
				if dbErr != nil {
					zap.S().Errorf("Failed to save deployment error status for %s: %v", ch.Name, dbErr)
				}
				return
			}

			containerInfos, err := ansible.ExtractContainerInfo(resultsBuff)
			if err != nil || len(containerInfos) == 0 {
				zap.S().Errorf("Failed to extract container info for %s: %v", ch.Name, err)
				dbErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "Failed to extract container info")
				if dbErr != nil {
					zap.S().Errorf("Failed to save deployment error status for %s: %v", ch.Name, dbErr)
				}
				return
			}

			connInfo, err := ansible.GetConnectionInfo(containerInfos, conf.Instancer.InstancerHost)
			if err != nil {
				zap.S().Errorf("Failed to build connection info for %s: %v", ch.Name, err)
				dbErr := models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusError, "", "Failed to build connection info")
				if dbErr != nil {
					zap.S().Errorf("Failed to save deployment error status for %s: %v", ch.Name, dbErr)
				}
				return
			}

			dbErr = models.UpdateDeploymentStatus(s.db, deployment, models.DeploymentStatusRunning, connInfo, "")
			if dbErr != nil {
				zap.S().Errorf("Failed to save deployment deployed status for %s: %v", ch.Name, dbErr)
				return
			}
			zap.S().Infof("Deployment of challenge %s completed successfully", ch.Name)
		}(chall)
	}

	return ctx.JSON(202, api.BulkOperationResponse{
		Message:         fmt.Sprintf("Deploying %d unique challenges", deployed),
		ChallengesCount: deployed,
		Challenges:      challenges,
	})
}

func (s *Server) TerminateAllAdminInstances(ctx echo.Context) error {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}

	deployments, err := models.GetAllUniqueDeployments(s.db)
	if err != nil {
		zap.S().Errorf("Failed to get unique deployments: %v", err)
		return ctx.JSON(500, api.Error{Message: utils.HTTP500Debug(fmt.Sprintf("Failed to get deployments: %v", err))})
	}

	if len(deployments) == 0 {
		return ctx.JSON(200, api.BulkOperationResponse{
			Message:         "No unique deployments found",
			ChallengesCount: 0,
		})
	}

	zap.S().Infof("Admin terminate-all request received for %d unique deployments", len(deployments))
	terminated := 0

	conf := config.Get()
	challenges := make([]api.ChallengeCategoryResponse, 0)
	for _, deployment := range deployments {
		if deployment.Status == models.DeploymentStatusStopping || deployment.Status == models.DeploymentStatusStopped {
			continue
		}

		err = models.UpdateDeploymentStatus(s.db, &deployment, models.DeploymentStatusStopping, "", "")
		if err != nil {
			zap.S().Errorf("Failed to update status for deployment %d: %v", deployment.ID, err)
			continue
		}
		challenges = append(challenges, api.ChallengeCategoryResponse{
			Category:      deployment.Category,
			ChallengeName: deployment.ChallengeName,
		})
		terminated++
		s.wg.Add(1)
		go func(d models.Deployment) {
			defer s.wg.Done()
			err := models.TerminateDeployment(s.db, s.challIdx, conf, &d)
			if err != nil {
				zap.S().Errorf("Failed to terminate deployment %d: %v", d.ID, err)
			}
		}(deployment)
	}

	return ctx.JSON(200, api.BulkOperationResponse{
		Message:         fmt.Sprintf("Terminating %d unique deployments", terminated),
		ChallengesCount: terminated,
		Challenges:      challenges,
	})
}
