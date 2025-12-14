package pkg

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"maps"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/28Pollux28/poly-instancer/pkg/api"
	"github.com/28Pollux28/poly-instancer/pkg/config"
	"github.com/28Pollux28/poly-instancer/pkg/models"
	"github.com/28Pollux28/poly-instancer/pkg/utils"
	"github.com/apenella/go-ansible/v2/pkg/execute"
	results "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"github.com/apenella/go-ansible/v2/pkg/execute/result/transformer"
	"github.com/apenella/go-ansible/v2/pkg/execute/stdoutcallback"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	yaml "github.com/oasdiff/yaml3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Server implements api.ServerInterface
type Server struct {
	db *gorm.DB
	mu sync.RWMutex
}

// Claims matches the JWT structure
type Claims struct {
	TeamID        string `json:"team_id"`
	ChallengeName string `json:"challenge_name"`
	Role          string `json:"role"`
	jwt.RegisteredClaims
}

type Challenge struct {
	PlaybookName     string                 `yaml:"playbook_name"`
	DeployParameters map[string]interface{} `yaml:"deploy_parameters"`
}

func NewServer(dbPath string) *Server {
	db, err := initDB(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	s := &Server{
		db: db,
	}

	//s.SyncMetrics()

	return s
}

func initDB(dbPath string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.New(log.Default(), logger.Config{
			LogLevel: logger.Info,
		}),
	})
	if err != nil {
		return nil, err
	}

	return db, db.AutoMigrate(models.Deployment{})
}

//// SyncMetrics queries Docker to find actual running state and updates Prometheus
//func (s *Server) SyncMetrics() {
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//
//	containers, err := s.DockerCli.ContainerList(ctx, client.ContainerListOptions{All: true})
//	if err != nil {
//		log.Printf("Failed to sync metrics: %v", err)
//		return
//	}
//
//	count := 0.0
//	for _, c := range containers.Items {
//		// Our naming convention is /chall_{challId}_{userId}
//		// Docker names return with a leading slash usually
//		for _, name := range c.Names {
//			if strings.Contains(name, "chall_") && c.State == "running" {
//				count++
//				break
//			}
//			//TODO remove
//			count++
//			activeInstancesPerTeam.WithLabelValues(name).Set(count)
//		}
//	}
//	activeInstances.Set(count)
//	log.Printf("Metrics synced. Found %.0f active challenge containers.", count)
//}

func getClaims(ctx echo.Context) (*Claims, error) {
	token, ok := ctx.Get("user").(*jwt.Token)
	if !ok {
		return nil, fmt.Errorf("invalid token")
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("invalid claims")
	}
	return claims, nil
}

func (s *Server) GetHealth(ctx echo.Context) error {
	return ctx.JSON(200, map[string]string{"status": "ok"})
}

func (s *Server) DeployInstance(ctx echo.Context) error {
	claims, err := getClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	var req api.DeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	log.Printf("Deploy request received for challenge %s for team %s", req.ChallengeName, claims.TeamID)
	// Check if challenge is valid for team
	if req.ChallengeName != claims.ChallengeName && claims.Role != "admin" {
		log.Printf("Unauthorized attempt to deploy challenge %s for team %s", req.ChallengeName, claims.TeamID)
		unauthorizedDeploymentsRequestsPerTeam.WithLabelValues(claims.TeamID).Inc()
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	var existingDeployment models.Deployment
	if err := s.db.Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).First(&existingDeployment).Error; err == nil {
		return ctx.JSON(409, api.Error{Message: utils.Ptr("Challenge already deployed for this team")})
	}
	conf := config.Get()

	challenge, params, err := getChallengeInfos(req.ChallengeName)
	if err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr(fmt.Sprintf("Failed to get challenge info: %v", err))})
	}

	deployment := models.Deployment{
		TeamID:        claims.TeamID,
		ChallengeName: req.ChallengeName,
		Status:        models.DeploymentStatusStarting,
	}

	if err := s.db.Create(&deployment).Error; err != nil {
		return ctx.JSON(500, api.Error{Message: utils.Ptr("Failed to create deployment record")})
	}

	projectName := "polypwn_" + req.ChallengeName + "_" + claims.TeamID
	sum := sha1.New().Sum([]byte(projectName))
	hexSum := hex.EncodeToString(sum)
	projectName = projectName + "_" + hexSum[:6]

	// Ansible Deploy
	// TODO: Use inventory from file
	playbookOpts := &playbook.AnsiblePlaybookOptions{
		ExtraVars: map[string]interface{}{
			"ansible_python_interpreter": "/usr/bin/python3",
			"compose_project":            projectName,
			"team_id":                    claims.TeamID,
			"challenge_name":             req.ChallengeName,
		},
		Inventory:   "151.145.32.232,",
		Connection:  "ssh",
		PrivateKey:  "/home/vlemaire/.ssh/ansible",
		User:        "ansible",
		VerboseVVVV: true,
		Tags:        "create",
	}
	// Add deploy parameters to extra vars
	maps.Copy(playbookOpts.ExtraVars, params)

	pbCmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(path.Join(conf.Deployer.AnsibleDir, req.ChallengeName, challenge.PlaybookName+".yaml")),
		playbook.WithPlaybookOptions(playbookOpts),
	)

	resultsBuff := bytes.NewBuffer([]byte{})

	executor := stdoutcallback.NewJSONStdoutCallbackExecute(
		execute.NewDefaultExecute(
			execute.WithCmd(pbCmd),
			execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
			execute.WithWrite(resultsBuff),
			execute.WithTransformers(
				transformer.Prepend("ansible-playbook"),
			),
		),
	)

	go func() {
		deployOps.Inc()
		if err := executor.Execute(context.Background()); err != nil {
			log.Printf("Ansible deploy failed: %v", err)
			s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Updates(models.Deployment{Status: models.DeploymentStatusError, Error: err.Error()})
			res, err := results.ParseJSONResultsStream(resultsBuff)
			if err != nil {
				log.Printf("Failed to parse Ansible results: %v", err)
			}
			log.Printf("Ansible deploy fail reason: %s", res.String())
			return
		}

		containerInfos, err := utils.ExtractContainerInfo(resultsBuff)
		if err != nil {
			log.Printf("Failed to extract container info: %v", err)
		}

		if len(containerInfos) == 0 {
			log.Printf("No container info found in Ansible results for challenge %s for team %s", req.ChallengeName, claims.TeamID)
			s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Updates(models.Deployment{Status: models.DeploymentStatusError, Error: "No container info found in Ansible results"})
			return
		}
		connInfo, err := s.getConnectionInfo(containerInfos, conf.Deployer.DeployerHost)
		if err != nil {
			log.Printf("Failed to build connection info: %v", err)
			s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Updates(models.Deployment{Status: models.DeploymentStatusError, Error: err.Error()})
			return
		}

		if err := s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Updates(models.Deployment{Status: models.DeploymentStatusRunning, ConnectionInfo: connInfo, Error: ""}).Error; err != nil {
			log.Printf("Failed to save deployment status: %v", err)
			return
		}
		log.Printf("Deployment of challenge %s for team %s completed successfully.", req.ChallengeName, claims.TeamID)
		activeInstancesPerTeam.WithLabelValues(claims.TeamID).Inc()
	}()

	return ctx.NoContent(202)
}

func getChallengeInfos(challengeName string) (chall Challenge, params map[string]interface{}, err error) {
	// Read challenge file
	conf := config.Get()
	challengeDirectory := conf.Deployer.ChallengeDir
	log.Printf("Reading challenge file from %s", challengeDirectory)
	challengeFilePath := path.Join(challengeDirectory, challengeName, "challenge.yaml")
	log.Printf("Challenge file path: %s", challengeFilePath)
	challengeData, err := os.ReadFile(challengeFilePath)
	if err != nil {
		return Challenge{}, nil, fmt.Errorf("failed to read challenge file: %w", err)
	}
	var challenge Challenge
	err = yaml.Unmarshal(challengeData, &challenge)
	if err != nil {
		return Challenge{}, nil, fmt.Errorf("failed to parse challenge file: %w", err)
	}

	if challenge.PlaybookName == "" {
		return Challenge{}, nil, fmt.Errorf("missing playbook_name in challenge file")
	}

	params = challenge.DeployParameters
	if params == nil {
		return Challenge{}, nil, fmt.Errorf("missing deploy_parameters in challenge file")
	}

	err = validatePlaybookParams(challenge.PlaybookName, params)
	if err != nil {
		return Challenge{}, nil, fmt.Errorf("invalid deploy_parameters: %w", err)
	}
	return challenge, params, nil
}

func (s *Server) getConnectionInfo(containerInfos []utils.ContainerInfo, host string) (string, error) {
	traefikLabel := ""
	for _, ci := range containerInfos {
		for _, label := range ci.Labels {
			if strings.HasPrefix(label, "traefik.http.routers.") {
				traefikLabel = label
				break
			}
		}
		// Check if we have a traefik label
		if traefikLabel != "" {
			label_parts := strings.Split(traefikLabel, "`")
			if len(label_parts) >= 2 {
				domainName := "https://" + label_parts[1] + "/"
				return domainName, nil
			}
		}
		// Check if we have published ports
		ports := []string{}
		for _, pub := range ci.Publishers {
			if pub.PublishedPort != 0 {
				// If IP is IPv6, continue
				if strings.Contains(pub.URL, ":") {
					continue
				}
				ports = append(ports, fmt.Sprintf("%s://%s:%d", pub.Protocol, host, pub.PublishedPort))
			}
		}
		if len(ports) > 0 {
			return strings.Join(ports, "\n"), nil
		}
	}
	return "", fmt.Errorf("no connection info found")
}

func (s *Server) GetInstanceStatus(ctx echo.Context) error {
	claims, err := getClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	log.Printf("Status request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)
	var deployment models.Deployment
	if err := s.db.Where("team_id = ? AND challenge_name = ?", claims.TeamID, claims.ChallengeName).First(&deployment).Error; err != nil {
		return ctx.JSON(404, api.Error{Message: utils.Ptr("No deployment found for this team and challenge")})
	}

	switch deployment.Status {
	case models.DeploymentStatusStarting:
		return ctx.JSON(200, api.StatusResponse{
			Status: utils.Ptr(api.StatusResponseStatusStarting),
		})
	case models.DeploymentStatusRunning:
		return ctx.JSON(200, api.StatusResponse{
			Status:         utils.Ptr(api.StatusResponseStatusRunning),
			ConnectionInfo: &deployment.ConnectionInfo,
		})
	case models.DeploymentStatusError:
		return ctx.JSON(500, api.Error{
			Message: utils.Ptr("An error occurred during deployment: " + deployment.Error + "\nAdministrators have been notified."),
		})
	case models.DeploymentStatusStopping:
		return ctx.JSON(400, api.StatusResponse{
			Status: utils.Ptr(api.StatusResponseStatusStopping),
		})
	}

	return ctx.JSON(200, api.StatusResponse{
		Status: utils.Ptr(api.StatusResponseStatusRunning),
	})
}

func (s *Server) TerminateInstance(ctx echo.Context) error {
	claims, err := getClaims(ctx)
	if err != nil {
		return ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	log.Printf("Terminate request received for challenge %s for team %s", claims.ChallengeName, claims.TeamID)

	var req api.DeployRequest
	if err := ctx.Bind(&req); err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Invalid request")})
	}
	log.Printf("Deploy request received for challenge %s for team %s", req.ChallengeName, claims.TeamID)
	// Check if challenge is valid for team
	if req.ChallengeName != claims.ChallengeName && claims.Role != "admin" {
		log.Printf("Unauthorized attempt to deploy challenge %s for team %s", req.ChallengeName, claims.TeamID)
		unauthorizedDeploymentsRequestsPerTeam.WithLabelValues(claims.TeamID).Inc()
		return ctx.JSON(403, api.Error{Message: utils.Ptr("Unauthorized")})
	}

	var deployment models.Deployment
	if err := s.db.Where("team_id = ? AND challenge_name = ?", claims.TeamID, claims.ChallengeName).First(&deployment).Error; err != nil {
		return ctx.JSON(404, api.Error{Message: utils.Ptr("No deployment found for this team and challenge")})
	}

	if deployment.Status == models.DeploymentStatusStopping {
		return ctx.JSON(400, api.Error{Message: utils.Ptr("Termination already in progress")})
	}

	conf := config.Get()

	challenge, params, err := getChallengeInfos(req.ChallengeName)
	if err != nil {
		return ctx.JSON(400, api.Error{Message: utils.Ptr(fmt.Sprintf("Failed to get challenge info: %v", err))})
	}

	s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, claims.ChallengeName).Update("status", models.DeploymentStatusStopping)

	projectName := "polypwn_" + req.ChallengeName + "_" + claims.TeamID
	sum := sha1.New().Sum([]byte(projectName))
	hexSum := hex.EncodeToString(sum)
	projectName = projectName + "_" + hexSum[:6]

	// Ansible Deploy
	// TODO: Use inventory from file
	playbookOpts := &playbook.AnsiblePlaybookOptions{
		ExtraVars: map[string]interface{}{
			"ansible_python_interpreter": "/usr/bin/python3",
			"compose_project":            projectName,
			"team_id":                    claims.TeamID,
			"challenge_name":             req.ChallengeName,
		},
		Inventory:   "151.145.32.232,",
		Connection:  "ssh",
		PrivateKey:  "/home/vlemaire/.ssh/ansible",
		User:        "ansible",
		VerboseVVVV: true,
		Tags:        "delete",
	}
	// Add deploy parameters to extra vars
	maps.Copy(playbookOpts.ExtraVars, params)

	pbCmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(path.Join(conf.Deployer.AnsibleDir, req.ChallengeName, challenge.PlaybookName+".yaml")),
		playbook.WithPlaybookOptions(playbookOpts),
	)

	resultsBuff := bytes.NewBuffer([]byte{})

	executor := stdoutcallback.NewJSONStdoutCallbackExecute(
		execute.NewDefaultExecute(
			execute.WithCmd(pbCmd),
			execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
			execute.WithWrite(resultsBuff),
			execute.WithTransformers(
				transformer.Prepend("ansible-playbook"),
			),
		),
	)

	go func() {
		if err := executor.Execute(context.Background()); err != nil {
			log.Printf("Ansible undeploy failed: %v", err)
			s.db.Model(&models.Deployment{}).Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Updates(models.Deployment{Status: models.DeploymentStatusError, Error: err.Error()})
			res, err := results.ParseJSONResultsStream(resultsBuff)
			if err != nil {
				log.Printf("Failed to parse Ansible results: %v", err)
			}
			log.Printf("Ansible undeploy fail reason: %s", res.String())
			return
		}
		if err := s.db.Where("team_id = ? AND challenge_name = ?", claims.TeamID, req.ChallengeName).Delete(&models.Deployment{}).Error; err != nil {
			log.Printf("Failed to delete deployment: %v", err)
			return
		}
		log.Printf("Termination of challenge %s for team %s completed successfully.", req.ChallengeName, claims.TeamID)
		activeInstancesPerTeam.WithLabelValues(claims.TeamID).Dec()
	}()
	return ctx.NoContent(200)
}
