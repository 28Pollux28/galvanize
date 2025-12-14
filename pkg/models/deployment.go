package models

import "gorm.io/gorm"

var (
	DeploymentStatusRunning  = "running"
	DeploymentStatusStarting = "starting"
	DeploymentStatusStopping = "stopping"
	DeploymentStatusError    = "error"
)

type Deployment struct {
	gorm.Model
	ChallengeName  string `gorm:"index"`
	TeamID         string `gorm:"index"`
	Status         string
	ConnectionInfo string
	Error          string
}
