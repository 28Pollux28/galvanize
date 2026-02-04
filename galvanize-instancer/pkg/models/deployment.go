package models

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	DeploymentStatusRunning  = "running"
	DeploymentStatusStarting = "starting"
	DeploymentStatusStopping = "stopping"
	DeploymentStatusError    = "error"
	DeploymentStatusStopped  = "stopped"

	ErrNotFound = errors.New("deployment not found")
)

type Deployment struct {
	gorm.Model
	ChallengeName     string `gorm:"index"`
	Category          string
	TeamID            *string `gorm:"index"`
	Status            string
	ConnectionInfo    string
	Error             string
	ExpiresAt         *time.Time `gorm:"index"`
	TimeExtensionLeft int
}

func (deployment *Deployment) BeforeDelete(tx *gorm.DB) (err error) {
	tx.Model(deployment).Update("status", DeploymentStatusStopped)
	return tx.Error
}

func GetExpiredDeployments(db *gorm.DB) ([]Deployment, error) {
	var deployments []Deployment
	result := db.Where("status = ? AND expired_at <= ?", DeploymentStatusRunning, time.Now()).Find(&deployments)
	return deployments, result.Error
}

func GetDeployment(db *gorm.DB, category, challengeName, teamID string, lock bool) (*Deployment, error) {
	var deployment Deployment
	var result *gorm.DB
	if lock {
		result = db.Clauses(clause.Locking{
			Strength: "UPDATE",
		}).Where("category = ? AND challenge_name = ? AND team_id = ?", category, challengeName, teamID).Limit(1).Find(&deployment)
	} else {
		result = db.Where("category = ? AND challenge_name = ? AND team_id = ?", category, challengeName, teamID).Limit(1).Find(&deployment)
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	return &deployment, nil
}

func GetUniqueDeployment(db *gorm.DB, category, challengeName string, lock bool) (*Deployment, error) {
	var deployment Deployment
	var result *gorm.DB
	if lock {
		result = db.Clauses(clause.Locking{
			Strength: "UPDATE",
		}).Where("category = ? AND challenge_name = ? AND team_id is NULL", category, challengeName).Limit(1).Find(&deployment)
	} else {
		result = db.Where("Category = ? AND challenge_name = ? AND team_id is NULL", category, challengeName).Limit(1).Find(&deployment)
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	return &deployment, nil
}

func GetAllUniqueDeployments(db *gorm.DB) ([]Deployment, error) {
	var deployments []Deployment
	result := db.Where("team_id IS NULL").Find(&deployments)
	return deployments, result.Error
}

func CreateDeployment(db *gorm.DB, challengeName, teamID, category string, defaultDeploymentTTL time.Duration, maxExtensions int) (*Deployment, error) {
	deployment := &Deployment{
		ChallengeName:     challengeName,
		TeamID:            &teamID,
		Category:          category,
		Status:            DeploymentStatusStarting,
		ExpiresAt:         utils.Ptr(time.Now().Add(defaultDeploymentTTL)),
		TimeExtensionLeft: maxExtensions,
	}
	result := db.Create(deployment)
	return deployment, result.Error
}

func CreateUniqueDeployment(db *gorm.DB, challengeName, category string) (*Deployment, error) {
	deployment := &Deployment{
		ChallengeName: challengeName,
		Category:      category,
		Status:        DeploymentStatusStarting,
	}
	result := db.Create(deployment)
	return deployment, result.Error
}

func UpdateDeploymentStatus(db *gorm.DB, deployment *Deployment, status, connectionInfo, errMsg string) error {
	deployment.Status = status
	deployment.ConnectionInfo = connectionInfo
	deployment.Error = errMsg
	result := db.Save(deployment)
	return result.Error
}

func DeleteDeployment(db *gorm.DB, deployment *Deployment) error {
	result := db.Delete(deployment)
	return result.Error
}

func ExtendDeploymentExpiration(db *gorm.DB, deployment *Deployment, extension, extensionWindow time.Duration, maxExtensions int) error {
	if deployment.ExpiresAt == nil {
		return errors.New("deployment has no expiration time")
	}
	timeLeft := time.Until(*deployment.ExpiresAt)
	if timeLeft > extensionWindow {
		return errors.New("cannot extend: extension window not reached")
	}
	if timeLeft <= 0 {
		return errors.New("deployment already expired")
	}

	if maxExtensions > -1 {
		if deployment.TimeExtensionLeft == -1 {
			deployment.TimeExtensionLeft = maxExtensions
		}
		if deployment.TimeExtensionLeft <= 0 {
			return errors.New("no time extensions left")
		}
	}

	deployment.ExpiresAt = utils.Ptr(deployment.ExpiresAt.Add(extension))
	if maxExtensions > -1 {
		deployment.TimeExtensionLeft -= 1
	}
	return db.Save(&deployment).Error
}

func TerminateDeployment(db *gorm.DB, idx *challenge.ChallengeIndex, conf *config.Config, deployment *Deployment) error {
	challengeName := deployment.ChallengeName
	category := deployment.Category
	teamID := deployment.TeamID
	chall, err := idx.Get(category, challengeName)
	if err != nil {
		zap.S().Errorf("Failed to get challenge infos: %v", err)
		return fmt.Errorf("failed to get challenge infos: %w", err)
	}
	if teamID == nil {
		teamID = utils.Ptr("")
	}

	executor, resultsBuff := ansible.PreparePlaybook(conf, "delete", chall, *teamID, chall.DeployParameters)

	if err := executor.Execute(context.Background()); err != nil {
		zap.S().Errorf("Ansible undeploy failed: %v", err)
		dbErr := UpdateDeploymentStatus(db, deployment, DeploymentStatusError, "", err.Error())
		if dbErr != nil {
			zap.S().Errorf("Saving undeployment error status failed: %v", dbErr)
			return dbErr
		}
		res, err2 := json.ParseJSONResultsStream(resultsBuff)
		if err2 != nil {
			zap.S().Errorf("Failed to parse Ansible results: %v", err2)
		}
		zap.S().Errorf("Ansible undeploy fail reason: %s", res.String())
		return fmt.Errorf("ansible undeploy failed: %s", res.String())
	}

	err = DeleteDeployment(db, deployment)
	if err != nil {
		zap.S().Errorf("Failed to delete deployment record: %v", err)
		return err
	}
	if *teamID == "" {
		zap.S().Debugf("Termination of unique challenge %s completed successfully.", challengeName)
		return nil
	}
	zap.S().Debugf("Termination of challenge %s for team %p completed successfully.", challengeName, teamID)
	//pkg.activeInstancesPerTeam.WithLabelValues(teamID).Dec()

	return nil
}
