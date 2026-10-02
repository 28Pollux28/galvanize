package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type mockDeployer struct {
	mu          sync.Mutex
	terminated  []string
	terminateFn func() error
}

func (m *mockDeployer) Deploy(context.Context, *config.Config, *challenge.Challenge, string) (string, error) {
	return "", nil
}

func (m *mockDeployer) Terminate(_ context.Context, _ *config.Config, chall *challenge.Challenge, teamID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminated = append(m.terminated, chall.Category+"/"+chall.Name+":"+teamID)
	if m.terminateFn != nil {
		return m.terminateFn()
	}
	return nil
}

type mockIndexer struct {
	challs map[string]*challenge.Challenge
}

func (m *mockIndexer) Get(category, name string) (*challenge.Challenge, error) {
	if c, ok := m.challs[category+"/"+name]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("challenge not found: %s/%s", category, name)
}
func (m *mockIndexer) GetAllUnique() []*challenge.Challenge  { return nil }
func (m *mockIndexer) GetAll() []*challenge.Challenge        { return nil }
func (m *mockIndexer) BuildIndex(string) error               { return nil }
func (m *mockIndexer) Skipped() []challenge.SkippedChallenge { return nil }
func (m *mockIndexer) add(c *challenge.Challenge) *mockIndexer {
	m.challs[c.Category+"/"+c.Name] = c
	return m
}
func newIndexer() *mockIndexer { return &mockIndexer{challs: map[string]*challenge.Challenge{}} }

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(models.Deployment{}))
	return db
}

// expiredDeployment stores a running team deployment of web/login that
// expired a minute ago.
func expiredDeployment(t *testing.T, db *gorm.DB) *models.Deployment {
	t.Helper()
	team := "team1"
	d := &models.Deployment{
		ChallengeName: "login", Category: "web", TeamID: &team,
		Status: models.DeploymentStatusRunning, ExpiresAt: utils.Ptr(time.Now().Add(-time.Minute)),
	}
	require.NoError(t, db.Create(d).Error)
	return d
}

func newScheduler(db *gorm.DB) *ExpiryScheduler {
	return NewExpiryScheduler(db, nil, zap.NewNop().Sugar())
}

func TestTerminateDeployment_DirectWithoutQueue(t *testing.T) {
	db := newTestDB(t)
	d := expiredDeployment(t, db)
	deployer := &mockDeployer{}
	idx := newIndexer().add(&challenge.Challenge{Name: "login", Category: "web"})
	s := newScheduler(db).WithDirectTermination(idx, &config.StaticProvider{Cfg: &config.Config{}}, deployer)

	s.terminateDeployment(d.ID)

	assert.Equal(t, []string{"web/login:team1"}, deployer.terminated)
	_, err := models.GetDeploymentByID(db, d.ID)
	assert.True(t, errors.Is(err, models.ErrNotFound), "the deployment is deleted, got %v", err)
}

func TestTerminateDeployment_DirectFailureMarksError(t *testing.T) {
	db := newTestDB(t)
	d := expiredDeployment(t, db)
	deployer := &mockDeployer{terminateFn: func() error { return errors.New("ansible failed") }}
	idx := newIndexer().add(&challenge.Challenge{Name: "login", Category: "web"})
	s := newScheduler(db).WithDirectTermination(idx, &config.StaticProvider{Cfg: &config.Config{}}, deployer)

	s.terminateDeployment(d.ID)

	got, err := models.GetDeploymentByID(db, d.ID)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusError, got.Status)
	assert.Contains(t, got.Error, "ansible failed")
}

func TestTerminateDeployment_NoQueueNoDirectMarksError(t *testing.T) {
	db := newTestDB(t)
	d := expiredDeployment(t, db)

	newScheduler(db).terminateDeployment(d.ID)

	got, err := models.GetDeploymentByID(db, d.ID)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusError, got.Status)
	assert.Contains(t, got.Error, "no job queue configured")
}

func TestTerminateDeployment_ExtendedIsKept(t *testing.T) {
	db := newTestDB(t)
	d := expiredDeployment(t, db)
	require.NoError(t, db.Model(d).Update("expires_at", time.Now().Add(time.Hour)).Error)
	deployer := &mockDeployer{}
	s := newScheduler(db).WithDirectTermination(newIndexer(), &config.StaticProvider{Cfg: &config.Config{}}, deployer)

	s.terminateDeployment(d.ID)

	assert.Empty(t, deployer.terminated)
	got, err := models.GetDeploymentByID(db, d.ID)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusRunning, got.Status)
}
