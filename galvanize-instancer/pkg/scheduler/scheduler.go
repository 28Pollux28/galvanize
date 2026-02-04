package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
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
	idx            *challenge.ChallengeIndex
	l              *zap.SugaredLogger
}

func NewExpiryScheduler(db *gorm.DB, idx *challenge.ChallengeIndex, logger *zap.SugaredLogger) *ExpiryScheduler {
	return &ExpiryScheduler{
		db:             db,
		mu:             sync.Mutex{},
		lookahead:      1 * time.Minute,
		rescheduleChan: make(chan struct{}, 1),
		idx:            idx,
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
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var d models.Deployment
		s.l.Debugf("Acquiring lock for deployment %d", deploymentID)
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&d, deploymentID).Error; err != nil {
			return err
		}

		if time.Now().Before(*d.ExpiresAt) {
			s.l.Errorf("deployment %d was extended, skipping", deploymentID)
			return errors.New("deployment was extended")
		}

		if d.Status != models.DeploymentStatusRunning {
			return nil
		}
		s.l.Debugf("terminating deployment %d", deploymentID)
		err := models.UpdateDeploymentStatus(tx, &d, models.DeploymentStatusStopping, "", "")
		if err != nil {
			return fmt.Errorf("failed to update deployment status to stopping: %w", err)
		}

		return nil
	})
	if err != nil {
		s.l.Errorf("failed to terminate deployment %d: %v", deploymentID, err)
	}

	var d models.Deployment
	if err := s.db.First(&d, deploymentID).Error; err != nil {
		s.l.Errorf("failed to fetch deployment %d after transaction: %v", deploymentID, err)
		return
	}
	if err := s.performTermination(&d, s.db); err != nil {
		s.l.Errorf("failed to perform termination for deployment %d: %v", deploymentID, err)
	}
}

func (s *ExpiryScheduler) performTermination(d *models.Deployment, tx *gorm.DB) error {

	err := models.TerminateDeployment(tx, s.idx, config.Get(), d)
	if err != nil {
		return fmt.Errorf("failed to terminate deployment: %w", err)
	}

	return nil
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
