package ansible

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openFileDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("cannot count file descriptors here")
	}
	return len(entries)
}

// Each call used to open a connection pool that was never closed, leaking
// one SQLite connection and file descriptor per call
func TestPortBindingsStore_ReusesConnection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "galvanize.db")
	r := portRange{lo: 34000, hi: 34999}

	// First use opens the store
	_, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team0", []string{"22"}, r)
	require.NoError(t, err)
	before := openFileDescriptors(t)

	for i := range 100 {
		_ = loadPortBindingsFromDB(dbPath, "web/a:team0")
		savePortBindingsToDB(dbPath, "web/a:team1", map[string]int{"22": 34500 + i%10})
		clearPortBindingsFromDB(dbPath, "web/a:team1")
		_, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team0", []string{"22"}, r)
		require.NoError(t, err)
	}
	CleanupStalePortBindings(dbPath)

	assert.LessOrEqual(t, openFileDescriptors(t), before+1, "no connection opened per call")

	portBindingDBMu.Lock()
	db, err := openPortBindingDB(dbPath)
	again, err2 := openPortBindingDB(dbPath)
	portBindingDBMu.Unlock()
	require.NoError(t, err)
	require.NoError(t, err2)
	assert.Same(t, db, again, "one pool per database file")
}

// A store that cannot be created is reported, and not kept for later calls
func TestPortBindingsStore_OpenFailureNotCached(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing-dir", "galvanize.db")

	_, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team0", []string{"22"}, portRange{lo: 35000, hi: 35009})
	require.Error(t, err)

	portBindingDBMu.Lock()
	_, cached := portBindingDBs[dbPath]
	portBindingDBMu.Unlock()
	assert.False(t, cached)

	// Once the directory exists, the store opens
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	b, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team0", []string{"22"}, portRange{lo: 35000, hi: 35009})
	require.NoError(t, err)
	assert.Contains(t, b, "22")
}

// The file opens but its table cannot be created: the pool is closed and
// not kept
func TestPortBindingsStore_MigrationFailureNotCached(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "galvanize.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("this is not a SQLite database, just some text long enough to be read"), 0o600))

	portBindingDBMu.Lock()
	db, err := openPortBindingDB(dbPath)
	_, cached := portBindingDBs[dbPath]
	portBindingDBMu.Unlock()

	require.Error(t, err)
	assert.Nil(t, db)
	assert.False(t, cached)
}
