package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	pkgerrors "github.com/28Pollux28/galvanize/pkg/errors"
	"github.com/28Pollux28/galvanize/pkg/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const (
	// maxRetries is the maximum number of retries for transient errors
	maxRetries = 3
)

// Pool manages a pool of Ansible workers
type Pool struct {
	queue      *Queue
	db         *gorm.DB
	challIdx   challenge.ChallengeIndexer
	confProv   config.Provider
	deployer   ansible.Deployer
	logger     *zap.SugaredLogger
	numWorkers int

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// PoolConfig holds configuration for the worker pool
type PoolConfig struct {
	NumWorkers int
	Queue      *Queue
	DB         *gorm.DB
	ChallIdx   challenge.ChallengeIndexer
	ConfProv   config.Provider
	Deployer   ansible.Deployer
	Logger     *zap.SugaredLogger
}

// NewPool creates a new worker pool
func NewPool(cfg PoolConfig) *Pool {
	numWorkers := cfg.NumWorkers
	if numWorkers <= 0 {
		numWorkers = 10 // default
	}

	deployer := cfg.Deployer
	if deployer == nil {
		deployer = &ansible.AnsibleDeployer{}
	}

	return &Pool{
		queue:      cfg.Queue,
		db:         cfg.DB,
		challIdx:   cfg.ChallIdx,
		confProv:   cfg.ConfProv,
		deployer:   deployer,
		logger:     cfg.Logger,
		numWorkers: numWorkers,
	}
}

// Start launches the worker pool
func (p *Pool) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Set aggressive GC target - GC when heap grows 50% instead of default 100%
	// This trades CPU for memory, reducing overall memory footprint
	debug.SetGCPercent(50)

	p.logger.Infof("Starting worker pool with %d workers", p.numWorkers)

	for i := 0; i < p.numWorkers; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		p.wg.Add(1)
		go p.runWorker(ctx, workerID)
	}
}

// Stop gracefully shuts down the worker pool
func (p *Pool) Stop() {
	p.logger.Info("Stopping worker pool...")
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.logger.Info("Worker pool stopped")
}

// runWorker is the main loop for a single worker
func (p *Pool) runWorker(ctx context.Context, workerID string) {
	defer p.wg.Done()

	p.logger.Infof("Worker %s started", workerID)

	for {
		// Check if we should shut down
		select {
		case <-ctx.Done():
			p.logger.Infof("Worker %s shutting down", workerID)
			return
		default:
		}

		// Try to get a job (Dequeue has 1s internal timeout)
		job, err := p.queue.Dequeue(ctx, workerID)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled, shutdown
				p.logger.Infof("Worker %s shutting down", workerID)
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// No job available, loop again to check context
				continue
			}
			p.logger.Errorf("Worker %s failed to dequeue: %v", workerID, err)
			time.Sleep(1 * time.Second) // Back off on error
			continue
		}

		p.processJob(ctx, workerID, job)
		job = nil // Help GC

		// Aggressively free memory after each job since Ansible processes are memory-heavy
		runtime.GC()
		debug.FreeOSMemory()
	}
}

// jobTimeout is the maximum time a job can run before being cancelled
const jobTimeout = 10 * time.Minute

// processJob handles a single job
func (p *Pool) processJob(ctx context.Context, workerID string, job *Job) {
	p.logger.Infof("Worker %s processing job: %s (attempt %d)", workerID, job.ID, job.Retries+1)

	// Create a timeout context for this job
	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	var err error
	switch job.Type {
	case JobTypeDeploy:
		err = p.processDeploy(jobCtx, job)
	case JobTypeTerminate:
		err = p.processTerminate(jobCtx, job)
	default:
		p.logger.Errorf("Unknown job type: %s", job.Type)
		_ = p.queue.Fail(ctx, workerID, job)
		return
	}

	// Check if job timed out
	if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
		p.logger.Errorf("Worker %s: job %s timed out after %v", workerID, job.ID, jobTimeout)
		err = fmt.Errorf("job timed out after %v", jobTimeout)
	}

	if err != nil {
		if isErr, errPattern := pkgerrors.IsTransientErrorMsg(err); isErr && job.Retries < maxRetries {
			p.logger.Warnf("Worker %s: transient error %s for job %s, requeueing: %v", workerID, errPattern, job.ID, err)
			backoff := time.Duration(job.Retries+1) * 2 * time.Second
			time.Sleep(backoff)
			if requeueErr := p.queue.Requeue(ctx, workerID, job); requeueErr != nil {
				p.logger.Errorf("Failed to requeue job %s: %v", job.ID, requeueErr)
			}
			return
		}

		p.logger.Errorf("Worker %s: job %s failed permanently: %v", workerID, job.ID, err)
		_ = p.queue.Fail(ctx, workerID, job)
		return
	}

	// Success
	if err := p.queue.Complete(ctx, workerID, job); err != nil {
		p.logger.Errorf("Failed to mark job %s as complete: %v", job.ID, err)
	}
}

// processDeploy handles a deploy job
func (p *Pool) processDeploy(ctx context.Context, job *Job) error {
	chall, err := p.challIdx.Get(job.Category, job.ChallengeName)
	if err != nil {
		return fmt.Errorf("challenge not found: %w", err)
	}

	deployment, err := models.GetDeploymentByID(p.db, job.DeploymentID)
	if err != nil {
		return fmt.Errorf("deployment not found: %w", err)
	}

	conf := p.confProv.GetConfig()

	connInfo, err := p.deployer.Deploy(ctx, conf, chall, job.TeamID)
	if err != nil {
		// Update deployment status to error
		dbErr := models.UpdateDeploymentStatus(p.db, deployment, models.DeploymentStatusError, "", err.Error())
		if dbErr != nil {
			p.logger.Errorf("Failed to update deployment error status: %v", dbErr)
		}
		return err
	}

	// Success - update deployment
	if err := models.UpdateDeploymentStatus(p.db, deployment, models.DeploymentStatusRunning, connInfo, ""); err != nil {
		return fmt.Errorf("failed to update deployment status: %w", err)
	}

	p.logger.Infof("Deployment of %s/%s for team %s completed successfully", job.Category, job.ChallengeName, job.TeamID)
	return nil
}

// processTerminate handles a terminate job
func (p *Pool) processTerminate(ctx context.Context, job *Job) error {
	chall, err := p.challIdx.Get(job.Category, job.ChallengeName)
	if err != nil {
		return fmt.Errorf("challenge not found: %w", err)
	}

	conf := p.confProv.GetConfig()

	if err := p.deployer.Terminate(ctx, conf, chall, job.TeamID); err != nil {
		// Update deployment status to error if we can find it
		if job.DeploymentID > 0 {
			deployment, dbErr := models.GetDeploymentByID(p.db, job.DeploymentID)
			if dbErr == nil {
				_ = models.UpdateDeploymentStatus(p.db, deployment, models.DeploymentStatusError, "", err.Error())
			}
		}
		return err
	}

	// Success - delete the deployment from the database
	if job.DeploymentID > 0 {
		deployment, dbErr := models.GetDeploymentByID(p.db, job.DeploymentID)
		if dbErr == nil {
			if err := models.DeleteDeployment(p.db, deployment); err != nil {
				p.logger.Errorf("Failed to delete deployment %d: %v", job.DeploymentID, err)
				// Don't return error - the termination succeeded, just cleanup failed
			}
		}
	}

	p.logger.Infof("Termination of %s/%s for team %s completed successfully", job.Category, job.ChallengeName, job.TeamID)
	return nil
}
