package auth

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// Claims matches the JWT structure
type Claims struct {
	TeamID        string `json:"team_id"`
	ChallengeName string `json:"challenge_name"`
	Category      string `json:"category"`
	Role          string `json:"role"`
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
