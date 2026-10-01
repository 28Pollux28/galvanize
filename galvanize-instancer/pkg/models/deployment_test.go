package models

import (
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(Deployment{}))
	return db
}

// addDeployment stores a team deployment (a unique one when teamID is empty)
// with the given status, expiring at expiresAt (none when nil).
func addDeployment(t *testing.T, db *gorm.DB, name, teamID, status string, expiresAt *time.Time) {
	t.Helper()
	d := &Deployment{ChallengeName: name, Category: "web", Status: status, ExpiresAt: expiresAt}
	if teamID != "" {
		d.TeamID = &teamID
	}
	require.NoError(t, db.Create(d).Error)
}

func names(deployments []Deployment) []string {
	out := make([]string, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, d.ChallengeName)
	}
	return out
}

func TestGetExpiredDeployments(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	addDeployment(t, db, "expired-later", "team1", DeploymentStatusRunning, utils.Ptr(now.Add(-time.Minute)))
	addDeployment(t, db, "expired-first", "team2", DeploymentStatusRunning, utils.Ptr(now.Add(-time.Hour)))
	addDeployment(t, db, "not-yet", "team3", DeploymentStatusRunning, utils.Ptr(now.Add(time.Hour)))
	addDeployment(t, db, "starting", "team4", DeploymentStatusStarting, utils.Ptr(now.Add(-time.Hour)))
	addDeployment(t, db, "no-expiry", "team5", DeploymentStatusRunning, nil)
	addDeployment(t, db, "unique", "", DeploymentStatusRunning, utils.Ptr(now.Add(-time.Hour)))

	expired, err := GetExpiredDeployments(db)
	require.NoError(t, err)
	assert.Equal(t, []string{"expired-first", "expired-later"}, names(expired), "running team deployments past expiry, soonest first")
}

func TestGetDeploymentsExpiringBy(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	addDeployment(t, db, "in-1m", "team1", DeploymentStatusRunning, utils.Ptr(now.Add(time.Minute)))
	addDeployment(t, db, "in-1h", "team2", DeploymentStatusRunning, utils.Ptr(now.Add(time.Hour)))
	addDeployment(t, db, "past", "team3", DeploymentStatusRunning, utils.Ptr(now.Add(-time.Minute)))

	upcoming, err := GetDeploymentsExpiringBy(db, now.Add(10*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, []string{"past", "in-1m"}, names(upcoming))
}
