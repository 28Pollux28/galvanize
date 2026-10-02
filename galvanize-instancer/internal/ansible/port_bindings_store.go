package ansible

import (
	"fmt"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// portBindingDBMu serializes the port bindings store and guards
// portBindingDBs.
var portBindingDBMu sync.Mutex

// portBindingDBs holds one connection pool per database file, opened on
// first use and kept for the process lifetime.
var portBindingDBs = map[string]*gorm.DB{}

type portBindingRecord struct {
	DeploymentKey string    `gorm:"column:deployment_key;primaryKey"`
	ContainerPort string    `gorm:"column:container_port;primaryKey"`
	HostPort      int       `gorm:"column:host_port;not null;uniqueIndex"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (portBindingRecord) TableName() string {
	return "published_port_bindings"
}

func loadPortBindingsFromDB(dbPath, deploymentKey string) map[string]int {
	if dbPath == "" || deploymentKey == "" {
		return map[string]int{}
	}

	portBindingDBMu.Lock()
	defer portBindingDBMu.Unlock()

	db, err := openPortBindingDB(dbPath)
	if err != nil {
		return map[string]int{}
	}

	var records []portBindingRecord
	if err := db.Where("deployment_key = ?", deploymentKey).Find(&records).Error; err != nil {
		return map[string]int{}
	}

	bindings := map[string]int{}
	for _, record := range records {
		bindings[record.ContainerPort] = record.HostPort
	}
	return bindings
}

// ensureRandomPortBindingsInDB returns the host ports of the deployment's
// randomized container ports, reserving a free one of ports for each container
// port that has none yet. It fails when the store cannot be opened or ports
// has no free port left, rather than leaving the container port to an
// ephemeral host port outside the configured range.
func ensureRandomPortBindingsInDB(dbPath, deploymentKey string, containerPorts []string, ports portRange) (map[string]int, error) {
	if dbPath == "" || deploymentKey == "" || len(containerPorts) == 0 {
		return map[string]int{}, nil
	}

	portBindingDBMu.Lock()
	defer portBindingDBMu.Unlock()

	db, err := openPortBindingDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open port bindings store: %w", err)
	}

	result := map[string]int{}
	var existing []portBindingRecord
	if err := db.Where("deployment_key = ?", deploymentKey).Find(&existing).Error; err == nil {
		for _, rec := range existing {
			result[rec.ContainerPort] = rec.HostPort
		}
	}

	for _, containerPort := range containerPorts {
		if _, ok := result[containerPort]; ok {
			continue
		}

		hostPort := reserveRandomHostPort(db, deploymentKey, containerPort, ports)
		if hostPort == 0 {
			return nil, fmt.Errorf("no free host port left in the randomized port range %s for container port %s", ports, containerPort)
		}
		result[containerPort] = hostPort
	}

	return result, nil
}

// reserveRandomHostPort records a host port of ports for the container port
// and returns it, or 0 if none is free. Ports are tried at random, then in
// order, so a nearly full range is still used up.
func reserveRandomHostPort(db *gorm.DB, deploymentKey, containerPort string, ports portRange) int {
	if db == nil || deploymentKey == "" || containerPort == "" {
		return 0
	}

	// Fast path: another worker may have already inserted this mapping.
	var existing portBindingRecord
	if err := db.First(&existing, "deployment_key = ? AND container_port = ?", deploymentKey, containerPort).Error; err == nil {
		return existing.HostPort
	}

	for range 128 {
		if hostPort := tryReserveHostPort(db, deploymentKey, containerPort, ports.random()); hostPort != 0 {
			return hostPort
		}
	}

	// Random picks kept colliding: take the first port no binding uses
	var used []int
	if err := db.Model(&portBindingRecord{}).Where("host_port BETWEEN ? AND ?", ports.lo, ports.hi).Pluck("host_port", &used).Error; err != nil {
		return 0
	}
	taken := make(map[int]struct{}, len(used))
	for _, p := range used {
		taken[p] = struct{}{}
	}
	for port := ports.lo; port <= ports.hi; port++ {
		if _, ok := taken[port]; ok {
			continue
		}
		if hostPort := tryReserveHostPort(db, deploymentKey, containerPort, port); hostPort != 0 {
			return hostPort
		}
	}
	return 0
}

// tryReserveHostPort records hostPort for the container port unless another
// binding uses it. It returns the container port's host port, which another
// worker may have recorded meanwhile, or 0.
func tryReserveHostPort(db *gorm.DB, deploymentKey, containerPort string, hostPort int) int {
	if hostPort == 0 {
		return 0
	}
	record := portBindingRecord{
		DeploymentKey: deploymentKey,
		ContainerPort: containerPort,
		HostPort:      hostPort,
	}
	res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
	if res.Error == nil && res.RowsAffected == 1 {
		return hostPort
	}

	// If insert was ignored, check whether our target mapping now exists.
	var mapped portBindingRecord
	if err := db.First(&mapped, "deployment_key = ? AND container_port = ?", deploymentKey, containerPort).Error; err == nil {
		return mapped.HostPort
	}
	return 0
}

func savePortBindingsToDB(dbPath, deploymentKey string, bindings map[string]int) {
	if dbPath == "" || deploymentKey == "" || len(bindings) == 0 {
		return
	}

	portBindingDBMu.Lock()
	defer portBindingDBMu.Unlock()

	db, err := openPortBindingDB(dbPath)
	if err != nil {
		return
	}

	for containerPort, hostPort := range bindings {
		record := portBindingRecord{
			DeploymentKey: deploymentKey,
			ContainerPort: containerPort,
			HostPort:      hostPort,
		}
		_ = db.Save(&record).Error
	}
}

func clearPortBindingsFromDB(dbPath, deploymentKey string) {
	if dbPath == "" || deploymentKey == "" {
		return
	}

	portBindingDBMu.Lock()
	defer portBindingDBMu.Unlock()

	db, err := openPortBindingDB(dbPath)
	if err != nil {
		return
	}

	_ = db.Delete(&portBindingRecord{}, "deployment_key = ?", deploymentKey).Error
}

// CleanupStalePortBindings removes port binding rows that no longer map to a
// non-deleted deployment entry.
func CleanupStalePortBindings(dbPath string) {
	if dbPath == "" {
		return
	}

	portBindingDBMu.Lock()
	defer portBindingDBMu.Unlock()

	db, err := openPortBindingDB(dbPath)
	if err != nil {
		zap.S().Warnf("Failed to open DB for stale port binding cleanup: %v", err)
		return
	}

	res := db.Exec(`
		DELETE FROM published_port_bindings
		WHERE deployment_key NOT IN (
			SELECT
				category || '/' || challenge_name || ':' || COALESCE(team_id, '')
			FROM deployments
			WHERE deleted_at IS NULL
		)
	`)
	if res.Error != nil {
		zap.S().Warnf("Failed to cleanup stale port bindings: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		zap.S().Infof("Removed %d stale port binding rows", res.RowsAffected)
	}
}

// openPortBindingDB returns the connection pool of the store at dbPath,
// opening it and creating its table on first use. Callers hold
// portBindingDBMu. Opening a pool on every call, as before, leaked one
// SQLite connection and its file descriptor per call, since none was closed.
func openPortBindingDB(dbPath string) (*gorm.DB, error) {
	if db, ok := portBindingDBs[dbPath]; ok {
		return db, nil
	}
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&portBindingRecord{}); err != nil {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		return nil, err
	}
	portBindingDBs[dbPath] = db
	return db, nil
}
