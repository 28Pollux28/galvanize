package challenge

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	yaml "github.com/oasdiff/yaml3"
	"go.uber.org/zap"
)

// ChallengeIndexer is the interface for looking up and managing challenges.
// Consumers should depend on this interface rather than the concrete ChallengeIndex.
type ChallengeIndexer interface {
	Get(category, name string) (*Challenge, error)
	GetAllUnique() []*Challenge
	GetAll() []*Challenge
	BuildIndex(baseDir string) error
}

// Compile-time check that ChallengeIndex implements ChallengeIndexer.
var _ ChallengeIndexer = (*ChallengeIndex)(nil)

type ChallengeIndex struct {
	mu     sync.RWMutex
	challs map[string]*Challenge
}

type Challenge struct {
	Name             string                 `yaml:"name"`
	Category         string                 `yaml:"category"`
	PlaybookName     string                 `yaml:"playbook_name"`
	Type             string                 `yaml:"type"`
	Unique           bool                   `yaml:"-"`
	DeployParameters map[string]interface{} `yaml:"deploy_parameters"`
}

func NewChallengeIndex(baseDir string) (*ChallengeIndex, error) {
	idx := &ChallengeIndex{
		challs: make(map[string]*Challenge),
	}
	err := idx.BuildIndex(baseDir)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

func (idx *ChallengeIndex) BuildIndex(baseDir string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.challs = make(map[string]*Challenge)
	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "example") {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || (d.Name() != "challenge.yml" && d.Name() != "challenge.yaml") {
			return err
		}
		// Parse challenge.yml to get category and name
		chall, err := parseChallenge(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if chall.Type != "zync" {
			return filepath.SkipDir
		}
		key := chall.Category + "/" + chall.Name
		idx.challs[key] = chall
		zap.S().Infof("Registered challenge: %s", key)

		return filepath.SkipDir
	})
	return err
}

func (idx *ChallengeIndex) Get(category, name string) (*Challenge, error) {
	key := category + "/" + name
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	chall, ok := idx.challs[key]
	if !ok {
		return nil, fmt.Errorf("challenge not found: %s", key)
	}
	return chall, nil
}

func (idx *ChallengeIndex) GetAllUnique() []*Challenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var unique []*Challenge
	for _, chall := range idx.challs {
		if chall.Unique {
			unique = append(unique, chall)
		}
	}
	return unique
}

func (idx *ChallengeIndex) GetAll() []*Challenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	challs := make([]*Challenge, 0, len(idx.challs))
	for _, ch := range idx.challs {
		challs = append(challs, ch)
	}
	return challs
}

func parseChallenge(challengeFilePath string) (*Challenge, error) {
	data, err := os.ReadFile(challengeFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read challenge file: %w", err)
	}
	var challenge Challenge
	err = yaml.Unmarshal(data, &challenge)
	if err != nil {
		return nil, fmt.Errorf("failed to parse challenge file: %w", err)
	}
	if challenge.Name == "" {
		return nil, fmt.Errorf("missing name in challenge file")
	}
	if challenge.Category == "" {
		return nil, fmt.Errorf("missing category in challenge file")
	}
	if challenge.Type == "" {
		return nil, fmt.Errorf("missing type in challenge file")
	}
	if unique, ok := challenge.DeployParameters["unique"]; ok {
		if b, ok := unique.(bool); ok && b {
			challenge.Unique = true
		}
	}

	return &challenge, nil
}
