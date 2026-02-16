package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/worker"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ExpiryScheduler struct {
	db             *gorm.DB
	timer          *time.Timer
	mu             sync.Mutex
	lookahead      time.Duration
	upcoming       []models.Deployment
	rescheduleChan chan struct{}
	wg             sync.WaitGroup // track ongoing terminations
	jobQueue       *worker.Queue
	l              *zap.SugaredLogger
}

func NewExpiryScheduler(db *gorm.DB, jobQueue *worker.Queue, logger *zap.SugaredLogger) *ExpiryScheduler {
	return &ExpiryScheduler{
		db:             db,
		mu:             sync.Mutex{},
		lookahead:      1 * time.Minute,
		rescheduleChan: make(chan struct{}, 1),
		jobQueue:       jobQueue,
		l:              logger,
	}
}

func (s *ExpiryScheduler) Start(ctx context.Context) {
	s.l.Debug("starting expiry scheduler")
	s.fetchNextExpiries()

	ticker := time.NewTicker(s.lookahead / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.timer != nil {
				s.timer.Stop()
			}
			s.mu.Unlock()
			s.wg.Wait() // wait for ongoing terminations
			close(s.rescheduleChan)
			return

		case <-ticker.C:
			s.fetchNextExpiries()

		case <-s.rescheduleChan:
			s.nextExpiry()
		}
	}
}

func (s *ExpiryScheduler) fetchNextExpiries() {
	s.l.Debug("fetching upcoming expirations")
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}

	window := time.Now().Add(s.lookahead)

	var deployments []models.Deployment
	err := s.db.Where("status = ? AND expires_at IS NOT NULL AND expires_at <= ? AND team_id IS NOT NULL", // Not null excludes unique deployments
		"running", window).
		Order("expires_at ASC").
		Find(&deployments).Error

	if err != nil {
		s.l.Errorf("failed to fetch upcoming expirations: %v", err)
		return
	}

	s.upcoming = deployments

	if len(deployments) == 0 {
		return
	}

	next := deployments[0]
	s.l.Debugf("scheduling expiry for deployment %d at %s", next.ID, next.ExpiresAt)
	delay := time.Until(*next.ExpiresAt)
	if delay < 0 {
		delay = 0
	}

	s.timer = time.AfterFunc(delay, func() {
		s.handleExpiry(next.ID)
	})
}

func (s *ExpiryScheduler) nextExpiry() {
	s.l.Debug("rescheduling expiry")
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.upcoming) == 0 {
		return
	}

	next := s.upcoming[0]
	s.l.Debugf("scheduling expiry for deployment %d at %s", next.ID, next.ExpiresAt)
	delay := time.Until(*next.ExpiresAt)
	if delay < 0 {
		delay = 0
	}

	if s.timer != nil {
		s.timer.Stop()
	}

	s.timer = time.AfterFunc(delay, func() {
		s.handleExpiry(next.ID)
	})
}

func (s *ExpiryScheduler) handleExpiry(deploymentID uint) {
	s.wg.Add(1)
	defer s.wg.Done()
	s.l.Debugf("handling expiry for deployment %d", deploymentID)
	// Remove from upcoming and trigger reschedule BEFORE termination
	s.mu.Lock()
	s.removeFromUpcoming(deploymentID)
	s.mu.Unlock()

	// Immediately schedule next deployment
	s.triggerReschedule()

	// Launch termination
	s.terminateDeployment(deploymentID)

}

func (s *ExpiryScheduler) removeFromUpcoming(deploymentID uint) {
	for i, d := range s.upcoming {
		if d.ID == deploymentID {
			s.upcoming = append(s.upcoming[:i], s.upcoming[i+1:]...)
			return
		}
	}
}

func (s *ExpiryScheduler) terminateDeployment(deploymentID uint) {
	var deployment models.Deployment

	err := s.db.Transaction(func(tx *gorm.DB) error {
		s.l.Debugf("Acquiring lock for deployment %d", deploymentID)
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&deployment, deploymentID).Error; err != nil {
			return err
		}

		if time.Now().Before(*deployment.ExpiresAt) {
			s.l.Debugf("deployment %d was extended, skipping", deploymentID)
			return errors.New("deployment was extended")
		}

		if deployment.Status != models.DeploymentStatusRunning {
			return nil
		}
		s.l.Debugf("terminating deployment %d", deploymentID)
		err := models.UpdateDeploymentStatus(tx, &deployment, models.DeploymentStatusStopping, "", "")
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		s.l.Errorf("failed to update deployment %d status: %v", deploymentID, err)
		return
	}

	// If job queue is available, submit termination job
	if s.jobQueue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		teamID := ""
		if deployment.TeamID != nil {
			teamID = *deployment.TeamID
		}

		job := worker.NewTerminateJob(deployment.Category, deployment.ChallengeName, teamID, deployment.ID)
		if err := s.jobQueue.Enqueue(ctx, job); err != nil {
			s.l.Errorf("failed to enqueue termination job for deployment %d: %v", deploymentID, err)
			// Mark deployment as error since we couldn't submit the job
			_ = models.UpdateDeploymentStatus(s.db, &deployment, models.DeploymentStatusError, "", "failed to enqueue termination job: "+err.Error())
		} else {
			s.l.Infof("submitted termination job for deployment %d (team: %s, challenge: %s/%s)", deploymentID, teamID, deployment.Category, deployment.ChallengeName)
		}
	} else {
		s.l.Warnf("no job queue available, cannot terminate deployment %d", deploymentID)
		_ = models.UpdateDeploymentStatus(s.db, &deployment, models.DeploymentStatusError, "", "no job queue configured for termination")
	}
}

func (s *ExpiryScheduler) triggerReschedule() {
	select {
	case s.rescheduleChan <- struct{}{}:
	default:
	}
}

func (s *ExpiryScheduler) NotifyChange(deploymentID uint) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range s.upcoming {
		if d.ID == deploymentID {
			s.fetchNextExpiries()
			return
		}
	}
}
