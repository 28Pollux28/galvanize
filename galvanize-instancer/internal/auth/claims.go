package auth

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// ScopeManage is the scope an admin token needs for the repository, build,
// image and challenge-file endpoints. The CTF platform's server puts it in
// the tokens it uses itself, never in the ones it hands to browsers: the
// dashboard's admin token can then manage instances but not clone
// repositories, run builds on the hosts or read challenge files.
const ScopeManage = "manage"

// Claims matches the JWT structure
type Claims struct {
	TeamID        string `json:"team_id"`
	ChallengeName string `json:"challenge_name"`
	Category      string `json:"category"`
	Role          string `json:"role"`
	Scope         string `json:"scope,omitempty"`
	jwt.RegisteredClaims
}

func GetClaims(ctx echo.Context) (*Claims, error) {
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
