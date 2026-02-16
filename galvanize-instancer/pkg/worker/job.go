package worker

import (
	"encoding/json"
	"time"
)

// JobType represents the type of Ansible job
type JobType string

const (
	JobTypeDeploy    JobType = "deploy"
	JobTypeTerminate JobType = "terminate"
)

// Job represents an Ansible job to be executed by a worker
type Job struct {
	ID            string    `json:"id"`
	Type          JobType   `json:"type"`
	Category      string    `json:"category"`
	ChallengeName string    `json:"challenge_name"`
	TeamID        string    `json:"team_id"`
	DeploymentID  uint      `json:"deployment_id"`
	CreatedAt     time.Time `json:"created_at"`
	Retries       int       `json:"retries"`
}

// NewDeployJob creates a new deploy job
func NewDeployJob(category, challengeName, teamID string, deploymentID uint) *Job {
	return &Job{
		ID:            generateJobID(JobTypeDeploy, category, challengeName, teamID),
		Type:          JobTypeDeploy,
		Category:      category,
		ChallengeName: challengeName,
		TeamID:        teamID,
		DeploymentID:  deploymentID,
		CreatedAt:     time.Now(),
		Retries:       0,
	}
}

// NewTerminateJob creates a new terminate job
func NewTerminateJob(category, challengeName, teamID string, deploymentID uint) *Job {
	return &Job{
		ID:            generateJobID(JobTypeTerminate, category, challengeName, teamID),
		Type:          JobTypeTerminate,
		Category:      category,
		ChallengeName: challengeName,
		TeamID:        teamID,
		DeploymentID:  deploymentID,
		CreatedAt:     time.Now(),
		Retries:       0,
	}
}

func generateJobID(jobType JobType, category, challengeName, teamID string) string {
	return string(jobType) + ":" + category + "/" + challengeName + ":" + teamID
}

// Marshal serializes the job to JSON
func (j *Job) Marshal() ([]byte, error) {
	return json.Marshal(j)
}

// UnmarshalJob deserializes a job from JSON
func UnmarshalJob(data []byte) (*Job, error) {
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
